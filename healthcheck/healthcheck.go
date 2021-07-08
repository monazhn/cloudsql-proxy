// Copyright 2021 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package healthcheck tests and communicates the health of the Cloud SQL Auth proxy.
package healthcheck

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/GoogleCloudPlatform/cloudsql-proxy/logging"
	"github.com/GoogleCloudPlatform/cloudsql-proxy/proxy/proxy"
)

const (
	livenessPath = "/liveness"
	readinessPath = "/readiness"
	portNum = ":8080" // TODO(monazhn): Think about a good port number.
)

// Locks to synchronize reads and writes for each HC boolean. Separate locks
// for each boolean is ideal so more than one boolean can be read/written
// simultaneously.
var (
	readinessL = &sync.Mutex{}
	livenessL  = &sync.Mutex{}
	startupL   = &sync.Mutex{}
)

// HC is a type used to implement health checks for the proxy.
type HC struct {
	// live being true means the proxy is running; in the case of the proxy
	// being unexpectedly terminated, we should (re)start the proxy.
	// live is related to Kubernetes liveness probing.
	live  bool
	// ready being true means the proxy is ready to serve new traffic; in the 
	// case that ready is false, we should wait to send new traffic to the
	// proxy. The value of ready determines the success or failure of 
	// Kubernetes readiness probing.
	ready bool
	// started is a flag used to support readiness probing and should not be
	// confused for affecting startup probing. When started becomes true, the
	// proxy is done starting up.
	started bool
	// srv is a pointer to the HTTP server used to communicated proxy health.
	srv *http.Server
}

// NewHealthCheck initializes a HC object and exposes HTTP endpoints used to
// communicate proxy health.
func NewHealthCheck(proxyClient *proxy.Client) *HC {
	srv := &http.Server{
		Addr: portNum,
	}

	hc := &HC{
		live: true,
		srv:  srv,
	}

	// Handlers used to set up HTTP endpoints.
	http.HandleFunc(readinessPath, func(w http.ResponseWriter, _ *http.Request) {
		readinessL.Lock()
		hc.ready = readinessTest(proxyClient, hc)
		if !hc.ready {
			readinessL.Unlock()
			w.WriteHeader(500)
			w.Write([]byte("error\n"))
			return
		}
		readinessL.Unlock()

		w.WriteHeader(200)
		w.Write([]byte("ok\n"))
	})

	http.HandleFunc(livenessPath, func(w http.ResponseWriter, _ *http.Request) {
		livenessL.Lock()
		hc.live = livenessTest()
		if !hc.live {
			livenessL.Unlock()
			w.WriteHeader(500)
			w.Write([]byte("error\n"))
			return
		}
		livenessL.Unlock()

		w.WriteHeader(200)
		w.Write([]byte("ok\n"))
	})

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logging.Errorf("Failed to start endpoint(s): %v", err)
		}
	}()

	return hc
}

// CloseHealthCheck gracefully shuts down the HTTP server belonging to the HC
// object.
func (hc *HC) CloseHealthCheck() {
	if hc != nil {
		if err := hc.srv.Shutdown(context.Background()); err != nil {
			logging.Errorf("Failed to shut down health check: ", err)
		}
	}
}

// NotifyReadyForConnections indicates to the proxy's HC that has finished startup.
func (hc *HC) NotifyReadyForConnections() {
	if hc != nil {
		startupL.Lock()
		hc.started = true
		startupL.Unlock()
	}
}

// livenessTest returns true as long as the proxy is running.
func livenessTest() bool {
	return true
}

// readinessTest will check several criteria before determining the proxy is
// ready for new connections.
func readinessTest(proxyClient *proxy.Client, hc *HC) bool {
	// Mark as not ready until we reach the 'Ready for Connections' log.
	startupL.Lock()
	if !hc.started {
		startupL.Unlock()
		logging.Errorf("Readiness failed because proxy has not finished starting up.")
		return false
	}
	startupL.Unlock()

	// Mark as not ready if the proxy is at the optional MaxConnections limit.
	if proxyClient.MaxConnections > 0 && atomic.LoadUint64(&proxyClient.ConnectionsCounter) >= proxyClient.MaxConnections {
		logging.Errorf("Readiness failed because proxy has reached the maximum connections limit (%d).", proxyClient.MaxConnections)
		return false
	}

	return true
}
