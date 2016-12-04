/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"reflect"
	"sync"

	"google.golang.org/grpc"

	"github.com/foolusion/elwinprotos/elwin"
	"github.com/foolusion/elwinprotos/storage"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/extensions"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/util/flowcontrol"
)

var (
	ec  elwin.ElwinClient
	esc storage.ElwinStorageClient
)

func main() {
	s := &Server{}

	if elwinClientConn, err := grpc.Dial("elwin-grpc.svc.default.cluster.local:80", grpc.WithInsecure()); err != nil {
		log.Fatal(err)
	} else {
		defer elwinClientConn.Close()
		ec = elwin.NewElwinClient(elwinClientConn)
	}
	if storageClientConn, err := grpc.Dial("elwin-storage.svc.default.cluster.local:80", grpc.WithInsecure()); err != nil {
		log.Fatal(err)
	} else {
		defer storageClientConn.Close()
		esc = storage.NewElwinStorageClient(storageClientConn)
	}

	var ingClient client.IngressInterface
	if kubeClient, err := client.NewInCluster(); err != nil {
		log.Fatalf("Failed to create client: %v.", err)
	} else {
		ingClient = kubeClient.Extensions().Ingress(api.NamespaceAll)
	}
	rateLimiter := flowcontrol.NewTokenBucketRateLimiter(0.1, 1)
	known := &extensions.IngressList{}
	s.UpdateRules(known)

	// server
	go func() {
		l, err := net.Listen("tcp", ":8080")
		if err != nil {
			log.Fatal(err)
		}
		defer l.Close()
		log.Fatal(http.Serve(l, s))
	}()

	// Controller loop
	for {
		rateLimiter.Accept()
		ingresses, err := ingClient.List(api.ListOptions{})
		if err != nil {
			log.Printf("Error retrieving ingresses: %v", err)
			continue
		}
		if reflect.DeepEqual(ingresses.Items, known.Items) {
			continue
		}
		known = ingresses
		s.UpdateRules(known)
	}
}

type Server struct {
	mu    sync.RWMutex
	rules map[url.URL]http.Handler
}

func (s *Server) UpdateRules(il *extensions.IngressList) {
	rules := parseRules(il)
	s.mu.Lock()
	s.rules = rules
	s.mu.Unlock()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := s.handler(r); h != nil {
		h.ServeHTTP(w, r)
		return
	}
	http.Error(w, "Not found.", http.StatusNotFound)
}

func (s *Server) handler(req *http.Request) http.Handler {
	u, err := url.Parse("http://" + req.Host + req.URL.Path)
	if err != nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if h, ok := s.rules[*u]; ok {
		return h
	}
	return nil
}

func parseRules(il *extensions.IngressList) map[url.URL]http.Handler {
	var a map[url.URL]struct{}
	if resp, err := esc.All(context.Background(), &storage.AllRequest{Environment: storage.Production}); err != nil {
		log.Fatalf("All request failed")
	} else if resp.Namespaces == nil {
		log.Fatalf("resp.Namespaces is nil")
	} else {
		a = filter(resp.Namespaces)
	}
	handlers := make(map[url.URL]http.Handler, len(il.Items))
	for _, ing := range il.Items {
		for _, rule := range ing.Spec.Rules {
			for _, path := range rule.HTTP.Paths {
				if u, err := url.Parse("http://" + rule.Host + path.Path); err != nil {
					log.Fatal(err)
				} else if _, ok := a[*u]; !ok {
					handlers[*u] = defaultHandler(path.Backend)
				} else {
					handlers[*u] = elwinHandler(u, path.Backend)
				}
			}
		}
	}
	return handlers
}

func filter(ns []*storage.Namespace) map[url.URL]struct{} {
	var a map[url.URL]struct{}
	for _, n := range ns {
		if ns == nil {
			continue
		}
		for _, l := range n.Labels {
			if l == "ingress" {
				if u, err := url.Parse(n.Name); err != nil {
					log.Fatal(err)
				} else {
					a[*u] = struct{}{}
				}
			}
		}
	}
	return a
}

func defaultHandler(backend extensions.IngressBackend) http.Handler {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Host = changeHost(backend)
		},
	}
}

func changeHost(backend extensions.IngressBackend) string {
	var host = backend.ServiceName + ".svc.default.cluster.local"
	if port := backend.ServicePort.String(); port != "" {
		host += ":" + port
	}
	return host
}

func elwinHandler(u *url.URL, backend extensions.IngressBackend) http.Handler {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			if id, err := req.Cookie("elwin"); err != nil {
				req.URL.Host = changeHost(backend)
			} else if resp, err := ec.GetNamespaces(context.TODO(), &elwin.Identifier{TeamID: "ingress", UserID: id.Value}); err != nil {
				req.URL.Host = changeHost(backend)
			} else if resp == nil {
				req.URL.Host = changeHost(backend)
			} else if exps := resp.GetExperiments(); exps == nil {
				req.URL.Host = changeHost(backend)
			} else if exp, ok := exps[u.String()]; !ok {
				req.URL.Host = changeHost(backend)
			} else if len(exp.Params) != 1 {
				req.URL.Host = changeHost(backend)
			} else {
				req.URL.Host = exp.Params[0].Value
			}
		},
	}
}
