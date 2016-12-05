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
	log.Println("Starting Elwingress...")
	s := &Server{}

	if elwinClientConn, err := grpc.Dial("elwin-grpc.ato.svc.cluster.local:80", grpc.WithInsecure()); err != nil {
		log.Fatalf("couldn't connect to elwin: %s", err)
	} else {
		defer elwinClientConn.Close()
		ec = elwin.NewElwinClient(elwinClientConn)
	}
	if storageClientConn, err := grpc.Dial("elwin-storage.ato.svc.cluster.local:80", grpc.WithInsecure()); err != nil {
		log.Fatalf("couldn't connect to elwin-storage: %s", err)
	} else {
		defer storageClientConn.Close()
		esc = storage.NewElwinStorageClient(storageClientConn)
	}

	var ingClient client.IngressInterface
	if kubeClient, err := client.NewInCluster(); err != nil {
		log.Fatalf("Failed to create client: %v.", err)
	} else {
		ingClient = kubeClient.Extensions().Ingress("ato")
	}
	rateLimiter := flowcontrol.NewTokenBucketRateLimiter(0.1, 1)
	known := &extensions.IngressList{}
	s.UpdateRules(known)

	// server
	go func() {
		l, err := net.Listen("tcp", ":8080")
		if err != nil {
			log.Fatalf("couldn't open listener on port 8080: %s", err)
		}
		log.Println("Listening on :8080")
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
	rules map[string]http.Handler
}

type hostPort struct {
	host string
	path string
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
	hpp := hostPort{host: req.Host, path: req.URL.Path}
	log.Println(hpp.String())
	s.mu.RLock()
	defer s.mu.RUnlock()
	if h, ok := s.rules[hpp.String()]; ok {
		return h
	}
	return nil
}

func (h hostPort) String() string {
	if h.path == "" {
		h.path = "/"
	}
	return h.host + h.path
}

func parseRules(il *extensions.IngressList) map[string]http.Handler {
	var a map[string]struct{}
	if resp, err := esc.All(context.Background(), &storage.AllRequest{Environment: storage.Production}); err != nil {
		log.Fatalf("All request failed")
	} else if resp.Namespaces == nil {
		log.Fatalf("resp.Namespaces is nil")
	} else {
		a = filter(resp.Namespaces)
	}
	handlers := make(map[string]http.Handler, len(il.Items))
	for _, ing := range il.Items {
		for _, rule := range ing.Spec.Rules {
			for _, path := range rule.HTTP.Paths {
				hpp := hostPort{host: rule.Host, path: path.Path}
				log.Println(hpp.String(), a)
				if _, ok := a[hpp.String()]; !ok {
					handlers[hpp.String()] = defaultHandler(path.Backend)
				} else {
					handlers[hpp.String()] = elwinHandler(hpp.String(), path.Backend)
				}
			}
		}
	}
	return handlers
}

func filter(ns []*storage.Namespace) map[string]struct{} {
	a := make(map[string]struct{}, len(ns))
	for _, n := range ns {
		if ns == nil {
			continue
		}
		for _, l := range n.Labels {
			if l == "ingress" {
				a[n.Name] = struct{}{}
			}
		}
	}
	return a
}

func defaultHandler(backend extensions.IngressBackend) http.Handler {
	log.Printf("creating default handler for %s", backend.ServiceName)
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			log.Println("default handler")
			b, err := httputil.DumpRequest(req, false)
			if err != nil {
				log.Println("couldn't dump request")
			}
			log.Println(string(b))
			req.URL.Scheme = "http"
			req.URL.Host = changeHost(backend)
		},
	}
}

func changeHost(backend extensions.IngressBackend) string {
	var host = backend.ServiceName + ".ato.svc.cluster.local"
	if port := backend.ServicePort.String(); port != "" {
		host += ":" + port
	}
	return host
}

func elwinHandler(hostPath string, backend extensions.IngressBackend) http.Handler {
	log.Printf("creating elwin handler for %s", backend.ServiceName)
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			log.Println("elwin handler")
			b, err := httputil.DumpRequest(req, false)
			if err != nil {
				log.Printf("couldn't dump request")
			}
			log.Println(string(b))
			req.URL.Scheme = "http"
			if id, err := req.Cookie("elwin"); err != nil {
				req.URL.Host = changeHost(backend)
			} else if resp, err := ec.GetNamespaces(context.TODO(), &elwin.Identifier{TeamID: "ingress", UserID: id.Value}); err != nil {
				req.URL.Host = changeHost(backend)
			} else if resp == nil {
				req.URL.Host = changeHost(backend)
			} else if exps := resp.GetExperiments(); exps == nil {
				req.URL.Host = changeHost(backend)
			} else if exp, ok := exps[hostPath]; !ok {
				req.URL.Host = changeHost(backend)
			} else if len(exp.Params) != 1 {
				req.URL.Host = changeHost(backend)
			} else {
				req.URL.Host = exp.Params[0].Value
			}
		},
	}
}
