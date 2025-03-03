// Copyright 2023 The etcd Authors
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

// These tests are performance sensitive, addition of cluster proxy makes them unstable.
//go:build !cluster_proxy

package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/pkg/v3/stringutil"
	"go.etcd.io/etcd/tests/v3/framework/e2e"
	"go.etcd.io/etcd/tests/v3/framework/integration"
)

const (
	watchResponsePeriod = 100 * time.Millisecond
	watchTestDuration   = 5 * time.Second
	// TODO: Reduce maxWatchDelay when https://github.com/etcd-io/etcd/issues/15402 is addressed.
	maxWatchDelay = 2 * time.Second
	// Configure enough read load to cause starvation from https://github.com/etcd-io/etcd/issues/15402.
	// Tweaked to pass on GitHub runner. For local runs please increase parameters.
	// TODO: Increase when https://github.com/etcd-io/etcd/issues/15402 is fully addressed.
	numberOfPreexistingKeys = 100
	sizeOfPreexistingValues = 5000
	readLoadConcurrency     = 10
)

type testCase struct {
	name   string
	config e2e.EtcdProcessClusterConfig
}

var tcs = []testCase{
	{
		name:   "NoTLS",
		config: e2e.EtcdProcessClusterConfig{ClusterSize: 1},
	},
	{
		name:   "ClientTLS",
		config: e2e.EtcdProcessClusterConfig{ClusterSize: 1, Client: e2e.ClientConfig{ConnectionType: e2e.ClientTLS}},
	},
}

func TestWatchDelayForPeriodicProgressNotification(t *testing.T) {
	e2e.BeforeTest(t)
	for _, tc := range tcs {
		tc := tc
		tc.config.WatchProcessNotifyInterval = watchResponsePeriod
		t.Run(tc.name, func(t *testing.T) {
			clus, err := e2e.NewEtcdProcessCluster(context.Background(), t, e2e.WithConfig(&tc.config))
			require.NoError(t, err)
			defer clus.Close()
			c := newClient(t, clus, tc.config)
			require.NoError(t, fillEtcdWithData(context.Background(), c, numberOfPreexistingKeys, sizeOfPreexistingValues))

			ctx, cancel := context.WithTimeout(context.Background(), watchTestDuration)
			defer cancel()
			g := errgroup.Group{}
			continuouslyExecuteGetAll(ctx, t, &g, c)
			validateWatchDelay(t, c.Watch(ctx, "fake-key", clientv3.WithProgressNotify()))
			require.NoError(t, g.Wait())
		})
	}
}

func TestWatchDelayForManualProgressNotification(t *testing.T) {
	e2e.BeforeTest(t)
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			clus, err := e2e.NewEtcdProcessCluster(context.Background(), t, e2e.WithConfig(&tc.config))
			require.NoError(t, err)
			defer clus.Close()
			c := newClient(t, clus, tc.config)
			require.NoError(t, fillEtcdWithData(context.Background(), c, numberOfPreexistingKeys, sizeOfPreexistingValues))

			ctx, cancel := context.WithTimeout(context.Background(), watchTestDuration)
			defer cancel()
			g := errgroup.Group{}
			continuouslyExecuteGetAll(ctx, t, &g, c)
			g.Go(func() error {
				for {
					err := c.RequestProgress(ctx)
					if err != nil {
						if strings.Contains(err.Error(), "context deadline exceeded") {
							return nil
						} else {
							return err
						}
					}
					time.Sleep(watchResponsePeriod)
				}
			})
			validateWatchDelay(t, c.Watch(ctx, "fake-key"))
			require.NoError(t, g.Wait())
		})
	}
}

func TestWatchDelayForEvent(t *testing.T) {
	e2e.BeforeTest(t)
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			clus, err := e2e.NewEtcdProcessCluster(context.Background(), t, e2e.WithConfig(&tc.config))
			require.NoError(t, err)
			defer clus.Close()
			c := newClient(t, clus, tc.config)
			require.NoError(t, fillEtcdWithData(context.Background(), c, numberOfPreexistingKeys, sizeOfPreexistingValues))

			ctx, cancel := context.WithTimeout(context.Background(), watchTestDuration)
			defer cancel()
			g := errgroup.Group{}
			g.Go(func() error {
				i := 0
				for {
					_, err := c.Put(ctx, "key", fmt.Sprintf("%d", i))
					if err != nil {
						if strings.Contains(err.Error(), "context deadline exceeded") {
							return nil
						} else {
							return err
						}
					}
					time.Sleep(watchResponsePeriod)
				}
			})
			continuouslyExecuteGetAll(ctx, t, &g, c)
			validateWatchDelay(t, c.Watch(ctx, "key"))
			require.NoError(t, g.Wait())
		})
	}
}

func validateWatchDelay(t *testing.T, watch clientv3.WatchChan) {
	start := time.Now()
	var maxDelay time.Duration
	for range watch {
		sinceLast := time.Since(start)
		if sinceLast > watchResponsePeriod+maxWatchDelay {
			t.Errorf("Unexpected watch response delayed over allowed threshold %s, delay: %s", maxWatchDelay, sinceLast-watchResponsePeriod)
		} else {
			t.Logf("Got watch response, since last: %s", sinceLast)
		}
		if sinceLast > maxDelay {
			maxDelay = sinceLast
		}
		start = time.Now()
	}
	sinceLast := time.Since(start)
	if sinceLast > maxDelay && sinceLast > watchResponsePeriod+maxWatchDelay {
		t.Errorf("Unexpected watch response delayed over allowed threshold %s, delay: unknown", maxWatchDelay)
		t.Errorf("Test finished while in middle of delayed response, measured delay: %s", sinceLast-watchResponsePeriod)
		t.Logf("Please increase the test duration to measure delay")
	} else {
		t.Logf("Max delay: %s", maxDelay-watchResponsePeriod)
	}
}

func fillEtcdWithData(ctx context.Context, c *clientv3.Client, keyCount int, valueSize uint) error {
	g := errgroup.Group{}
	concurrency := 10
	keysPerRoutine := keyCount / concurrency
	for i := 0; i < concurrency; i++ {
		i := i
		g.Go(func() error {
			for j := 0; j < keysPerRoutine; j++ {
				_, err := c.Put(ctx, fmt.Sprintf("%d", i*keysPerRoutine+j), stringutil.RandString(valueSize))
				if err != nil {
					return err
				}
			}
			return nil
		})
	}
	return g.Wait()
}

func continuouslyExecuteGetAll(ctx context.Context, t *testing.T, g *errgroup.Group, c *clientv3.Client) {
	mux := sync.RWMutex{}
	size := 0
	for i := 0; i < readLoadConcurrency; i++ {
		g.Go(func() error {
			for {
				_, err := c.Get(ctx, "", clientv3.WithPrefix())
				if err != nil {
					if strings.Contains(err.Error(), "context deadline exceeded") {
						return nil
					} else {
						return err
					}
				}
				mux.Lock()
				size += numberOfPreexistingKeys * sizeOfPreexistingValues
				mux.Unlock()
			}
		})
	}
	g.Go(func() error {
		lastSize := size
		for range time.Tick(time.Second) {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			mux.RLock()
			t.Logf("Generating read load around %.1f MB/s", float64(size-lastSize)/1000/1000)
			lastSize = size
			mux.RUnlock()
		}
		return nil
	})
}

func newClient(t *testing.T, clus *e2e.EtcdProcessCluster, cfg e2e.EtcdProcessClusterConfig) *clientv3.Client {
	tlscfg, err := tlsInfo(t, cfg.Client)
	if err != nil {
		t.Fatal(err)
	}
	ccfg := clientv3.Config{
		Endpoints:   clus.EndpointsV3(),
		DialTimeout: 5 * time.Second,
		DialOptions: []grpc.DialOption{grpc.WithBlock()},
	}
	if tlscfg != nil {
		tls, err := tlscfg.ClientConfig()
		if err != nil {
			t.Fatal(err)
		}
		ccfg.TLS = tls
	}
	c, err := clientv3.New(ccfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close()
	})
	return c
}

func tlsInfo(t testing.TB, cfg e2e.ClientConfig) (*transport.TLSInfo, error) {
	switch cfg.ConnectionType {
	case e2e.ClientNonTLS, e2e.ClientTLSAndNonTLS:
		return nil, nil
	case e2e.ClientTLS:
		if cfg.AutoTLS {
			tls, err := transport.SelfCert(zap.NewNop(), t.TempDir(), []string{"localhost"}, 1)
			if err != nil {
				return nil, fmt.Errorf("failed to generate cert: %s", err)
			}
			return &tls, nil
		} else {
			return &integration.TestTLSInfo, nil
		}
	default:
		return nil, fmt.Errorf("config %v not supported", cfg)
	}
}
