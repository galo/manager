// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kube

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/pkg/util/intstr"

	"istio.io/manager/model"
	proxyconfig "istio.io/manager/model/proxy/alphav1/config"
	"istio.io/manager/test/mock"
)

func TestIngressIgnored(t *testing.T) {
	cases := []struct {
		ingressClass string
		shouldIgnore bool
	}{
		{ingressClass: "nginx", shouldIgnore: true},
		{ingressClass: "istio", shouldIgnore: false},
		{ingressClass: "Istio", shouldIgnore: false},
		{ingressClass: "", shouldIgnore: false},
	}

	for _, c := range cases {
		ingress := v1beta1.Ingress{
			ObjectMeta: v1.ObjectMeta{
				Name:        "test-ingress",
				Namespace:   "default",
				Annotations: make(map[string]string),
			},
			Spec: v1beta1.IngressSpec{
				Backend: &v1beta1.IngressBackend{
					ServiceName: "default-http-backend",
					ServicePort: intstr.FromInt(80),
				},
			},
		}

		if c.ingressClass != "" {
			ingress.Annotations["kubernetes.io/ingress.class"] = c.ingressClass
		}

		if c.shouldIgnore != ingressIgnored(&ingress) {
			t.Errorf("ingressIgnored(<ingress of class '%s'>) => %v, want %v",
				c.ingressClass, !c.shouldIgnore, c.shouldIgnore)
		}
	}
}

func TestController(t *testing.T) {
	cl := makeClient(t)
	ns := makeNamespace(cl.client, t)
	defer deleteNamespace(cl.client, ns)

	stop := make(chan struct{})
	defer close(stop)

	ctl := NewController(cl, ns, resync)
	added, deleted := 0, 0
	n := 5
	err := ctl.AppendConfigHandler(mock.Kind, func(k model.Key, o proto.Message, ev model.Event) {
		switch ev {
		case model.EventAdd:
			if deleted != 0 {
				t.Errorf("Events are not serialized (add)")
			}
			added++
		case model.EventDelete:
			if added != n {
				t.Errorf("Events are not serialized (delete)")
			}
			deleted++
		}
		glog.Infof("Added %d, deleted %d", added, deleted)
	})
	if err != nil {
		t.Error(err)
	}
	go ctl.Run(stop)

	mock.CheckMapInvariant(cl, t, ns, n)
	glog.Infof("Waiting till all events are received")
	eventually(func() bool { return added == n && deleted == n }, t)
}

func TestControllerCacheFreshness(t *testing.T) {
	cl := makeClient(t)
	ns := makeNamespace(cl.client, t)
	defer deleteNamespace(cl.client, ns)
	stop := make(chan struct{})
	ctl := NewController(cl, ns, resync)

	// test interface implementation
	var _ model.Controller = ctl

	var doneMu sync.Mutex
	done := false

	// validate cache consistency
	err := ctl.AppendConfigHandler(mock.Kind, func(k model.Key, v proto.Message, ev model.Event) {
		elts, _ := ctl.List(mock.Kind, ns)
		switch ev {
		case model.EventAdd:
			if len(elts) != 1 {
				t.Errorf("Got %#v, expected %d element(s) on ADD event", elts, 1)
			}
			glog.Infof("Calling Delete(%#v)", k)
			err := ctl.Delete(k)
			if err != nil {
				t.Error(err)
			}
		case model.EventDelete:
			if len(elts) != 0 {
				t.Errorf("Got %#v, expected zero elements on DELETE event", elts)
			}
			glog.Infof("Stopping channel for (%#v)", k)
			close(stop)
			doneMu.Lock()
			done = true
			doneMu.Unlock()
		}
	})
	if err != nil {
		t.Error(err)
	}

	go ctl.Run(stop)
	k := model.Key{Kind: mock.Kind, Name: "test", Namespace: ns}
	o := mock.Make(0)

	// add and remove
	glog.Infof("Calling Post(%#v)", k)
	if err := ctl.Post(k, o); err != nil {
		t.Error(err)
	}
	eventually(func() bool {
		doneMu.Lock()
		defer doneMu.Unlock()
		return done
	}, t)
}

func TestControllerClientSync(t *testing.T) {
	cl := makeClient(t)
	ns := makeNamespace(cl.client, t)
	n := 5
	defer deleteNamespace(cl.client, ns)
	stop := make(chan struct{})
	defer close(stop)

	keys := make(map[int]model.Key)
	// add elements directly through client
	for i := 0; i < n; i++ {
		keys[i] = model.Key{Name: fmt.Sprintf("test%d", i), Namespace: ns, Kind: mock.Kind}
		if err := cl.Post(keys[i], mock.Make(i)); err != nil {
			t.Error(err)
		}
	}

	// check in the controller cache
	ctl := NewController(cl, ns, resync)
	go ctl.Run(stop)
	eventually(func() bool { return ctl.HasSynced() }, t)
	os, _ := ctl.List(mock.Kind, ns)
	if len(os) != n {
		t.Errorf("ctl.List => Got %d, expected %d", len(os), n)
	}

	// remove elements directly through client
	for i := 0; i < n; i++ {
		if err := cl.Delete(keys[i]); err != nil {
			t.Error(err)
		}
	}

	// check again in the controller cache
	eventually(func() bool {
		os, _ = ctl.List(mock.Kind, ns)
		glog.Infof("ctl.List => Got %d, expected %d", len(os), 0)
		return len(os) == 0
	}, t)

	// now add through the controller
	for i := 0; i < n; i++ {
		if err := ctl.Post(keys[i], mock.Make(i)); err != nil {
			t.Error(err)
		}
	}

	// check directly through the client
	eventually(func() bool {
		cs, _ := ctl.List(mock.Kind, ns)
		os, _ := cl.List(mock.Kind, ns)
		glog.Infof("ctl.List => Got %d, expected %d", len(cs), n)
		glog.Infof("cl.List => Got %d, expected %d", len(os), n)
		return len(os) == n && len(cs) == n
	}, t)

	// remove elements directly through the client
	for i := 0; i < n; i++ {
		if err := cl.Delete(keys[i]); err != nil {
			t.Error(err)
		}
	}
}

func eventually(f func() bool, t *testing.T) {
	interval := 64 * time.Millisecond
	for i := 0; i < 10; i++ {
		if f() {
			return
		}
		glog.Infof("Sleeping %v", interval)
		time.Sleep(interval)
		interval = 2 * interval
	}
	t.Errorf("Failed to satisfy function")
}

const (
	testService = "test"
	resync      = 100 * time.Millisecond
)

func TestServices(t *testing.T) {
	cl := makeClient(t)
	ns := makeNamespace(cl.client, t)
	defer deleteNamespace(cl.client, ns)

	stop := make(chan struct{})
	defer close(stop)

	ctl := NewController(cl, ns, resync)
	go ctl.Run(stop)

	hostname := fmt.Sprintf("%s.%s.%s", testService, ns, ServiceSuffix)

	var sds model.ServiceDiscovery = ctl
	makeService(testService, ns, cl.client, t)
	eventually(func() bool {
		out := sds.Services()
		glog.Info("Services: %#v", out)
		return len(out) == 1 &&
			out[0].Hostname == hostname &&
			len(out[0].Ports) == 1 &&
			out[0].Ports[0].Protocol == model.ProtocolHTTP
	}, t)

	svc, exists := sds.GetService(hostname)
	if !exists {
		t.Errorf("GetService(%q) => %t, want true", hostname, exists)
	}
	if svc.Hostname != hostname {
		t.Errorf("GetService(%q) => %q", hostname, svc.Hostname)
	}

	missing := fmt.Sprintf("does-not-exist.%s.%s", ns, ServiceSuffix)
	_, exists = sds.GetService(missing)
	if exists {
		t.Errorf("GetService(%q) => %t, want false", missing, exists)
	}
}

func makeService(n, ns string, cl *kubernetes.Clientset, t *testing.T) {
	_, err := cl.Core().Services(ns).Create(&v1.Service{
		ObjectMeta: v1.ObjectMeta{Name: n},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Port: 80,
					Name: "http-example",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf(err.Error())
	}
	glog.Infof("Created service %s", n)
}

func TestIstioConfig(t *testing.T) {
	cl := makeClient(t)
	ns := makeNamespace(cl.client, t)
	defer deleteNamespace(cl.client, ns)

	rule := &proxyconfig.RouteRule{
		Destination: "foo",
		Match: &proxyconfig.MatchCondition{
			HttpHeaders: map[string]*proxyconfig.StringMatch{
				"uri": {
					MatchType: &proxyconfig.StringMatch_Exact{
						Exact: "test",
					},
				},
			},
		},
	}

	key := model.Key{Kind: model.RouteRule, Name: "test", Namespace: ns}

	if err := cl.Post(key, rule); err != nil {
		t.Errorf("cl.Post() => error %v, want no error", err)
	}

	out, exists := cl.Get(key)
	if !exists {
		t.Errorf("cl.Get() => missing")
		return
	}

	if !reflect.DeepEqual(rule, out) {
		t.Errorf("cl.Get(%v) => %v, want %v", key, out, rule)
	}

	registry := model.IstioRegistry{ConfigRegistry: cl}

	rules := registry.RouteRules(ns)
	if len(rules) != 1 || !reflect.DeepEqual(rules[key], rule) {
		t.Errorf("RouteRules() => %v, want %v", rules, rule)
	}

	destinations := registry.PoliciesByNamespace(ns)
	if len(destinations) > 0 {
		t.Errorf("DestinationPolicies() => %v, want empty", destinations)
	}
}
