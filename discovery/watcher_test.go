package discovery

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/consul/proto-public/pbdataplane"
	"github.com/hashicorp/consul/proto-public/pbserverdiscovery"
	"github.com/hashicorp/consul/sdk/testutil"
	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"
)

const testServerManagementToken = "12345678-90ab-cdef-0000-12345678abcd"

// TestRun starts a Consul server cluster and starts a Watcher.
func TestRun(t *testing.T) {
	cases := map[string]struct {
		config               Config
		serverConfigFn       testutil.ServerConfigCallback
		testWithServerEvalFn bool
	}{
		"no acls": {
			config:               Config{},
			testWithServerEvalFn: true,
		},
		"static token": {
			config: Config{
				Credentials: Credentials{
					Type: CredentialsTypeStatic,
					Static: StaticTokenCredential{
						Token: testServerManagementToken,
					},
				},
			},
			serverConfigFn: enableACLsConfigFn,
		},
		"server watch disabled": {
			config: Config{
				ServerWatchDisabled:         true,
				ServerWatchDisabledInterval: 1 * time.Second,
			},
			testWithServerEvalFn: true,
		},
	}
	for name, c := range cases {
		c := c

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		t.Cleanup(cancel)

		wasServerEvalFnCalled := false
		if c.testWithServerEvalFn {
			c.config.ServerEvalFn = func(state State) bool {
				require.NotNil(t, state.GRPCConn)
				require.NotEmpty(t, state.Address.String())
				require.NotEmpty(t, state.DataplaneFeatures)
				wasServerEvalFnCalled = true
				return true
			}
		}

		// The gRPC balancer registry is global and not thread safe. gRPC starts goroutine(s)
		// that read from the balancer registry when building balancers, and expects all writes
		// to the registry to occur synchronously upfront in an init() function.
		//
		// To avoid the race detector in parallel tests, we must have all balancer.Register calls
		// that write to the registry happen prior to starting any Watchers. This means we must
		// construct all Watchers first, synchronously and before Watcher.Run called.
		w, err := NewWatcher(ctx, c.config, hclog.New(&hclog.LoggerOptions{
			Name:  fmt.Sprintf("watcher/%s", name),
			Level: hclog.Debug,
		}))
		require.NoError(t, err)
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			servers := startConsulServers(t, 3, c.serverConfigFn)

			// To test with local Consul servers, we inject custom server ports. The Config struct,
			// go-netaddrs, and the server watch stream do not support per-server ports.
			w.discoverer = servers
			w.nodeToAddrFn = servers.nodeToAddrFn

			// Start the Watcher.
			subscribeChan := w.Subscribe()
			go w.Run()
			t.Cleanup(w.Stop)

			// Get initial state. This blocks until initialization is complete.
			initialState, err := w.State()
			require.NoError(t, err)
			require.NotNil(t, initialState, initialState.GRPCConn)
			require.Contains(t, servers.servers, initialState.Address.String())

			// Make sure the ServerEvalFn is called (or not).
			require.Equal(t, c.testWithServerEvalFn, wasServerEvalFnCalled)

			// Check we can also get state this way.
			require.Equal(t, initialState, receiveSubscribeState(t, ctx, subscribeChan))

			// check the token we get back.
			switch c.config.Credentials.Type {
			case CredentialsTypeStatic:
				require.Equal(t, initialState.Token, testServerManagementToken)
			case CredentialsTypeLogin:
				require.FailNow(t, "TODO: support acl token login")
			default:
				require.Equal(t, initialState.Token, "")
			}

			unaryClient := pbdataplane.NewDataplaneServiceClient(initialState.GRPCConn)
			unaryRequest := func(t require.TestingT) {
				req := &pbdataplane.GetSupportedDataplaneFeaturesRequest{}
				resp, err := unaryClient.GetSupportedDataplaneFeatures(ctx, req)
				require.NoError(t, err, "error from unary request")
				require.NotNil(t, resp)
			}

			streamClient := pbserverdiscovery.NewServerDiscoveryServiceClient(initialState.GRPCConn)
			streamRequest := func(t require.TestingT) {
				// It seems like the stream will not automatically switch servers via the resolver.
				// It gets an address once when the stream is created.
				stream, err := streamClient.WatchServers(ctx, &pbserverdiscovery.WatchServersRequest{})
				require.NoError(t, err, "opening stream")
				_, err = stream.Recv()
				require.NoError(t, err, "error from stream")
			}

			// Make a gRPC request to check that the gRPC connection is working.
			// This validates that the custom interceptor is injecting the ACL token.
			unaryRequest(t)
			streamRequest(t)

			servers.stopServer(t, initialState.Address)

			// Wait for the server switch.
			stateAfterStop := receiveSubscribeState(t, ctx, subscribeChan)
			require.NotEmpty(t, stateAfterStop.Address.String())
			require.NotEqual(t, stateAfterStop.Address, initialState.Address)

			// Check we can also get state this way.
			state, err := w.State()
			require.Equal(t, stateAfterStop, state)

			// Check requests work.
			unaryRequest(t)
			streamRequest(t)

			// Tell the Watcher to switch servers.
			w.requestServerSwitch()
			stateAfterSwitch := receiveSubscribeState(t, ctx, subscribeChan)

			// Check the server changed.
			require.NoError(t, err)
			require.NotEmpty(t, stateAfterStop.Address.String())
			require.NotEqual(t, stateAfterStop.Address, initialState.Address)

			// Check we can also get state this way.
			state, err = w.State()
			require.NoError(t, err)
			require.Equal(t, stateAfterSwitch, state)

			unaryRequest(t)
			streamRequest(t)

			t.Logf("test successful")
		})
	}
}

func receiveSubscribeState(t *testing.T, ctx context.Context, ch <-chan State) State {
	select {
	case val := <-ch:
		return val
	case <-ctx.Done():
		require.Failf(t, "failed to receive from channel", "error=%s", ctx.Err())
	}
	return State{}
}

type consulServers struct {
	servers map[string]*testutil.TestServer
	sync.Mutex
}

// Implement a custom Discoverer to inject addresses with custom ports, so that
// we can use multiple local Consul test servers. go-netaddrs doesn't support
// per-server ports.
var _ Discoverer = (*consulServers)(nil)

func (c *consulServers) Discover(ctx context.Context) ([]Addr, error) {
	return c.grpcAddrs()
}

func (c *consulServers) grpcAddrs() ([]Addr, error) {
	c.Lock()
	defer c.Unlock()

	var addrs []Addr
	for _, srv := range c.servers {
		addr, err := MakeAddr(srv.Config.Bind, srv.Config.Ports.GRPC)
		if err != nil {
			return nil, err
		}
		addrs = append(addrs, addr)
	}
	return addrs, nil
}

func (c *consulServers) nodeToAddrFn(nodeID, addr string) (Addr, error) {
	c.Lock()
	defer c.Unlock()

	for _, srv := range c.servers {
		if srv.Config.NodeID == nodeID {
			return MakeAddr(addr, srv.Config.Ports.GRPC)
		}
	}
	return Addr{}, fmt.Errorf("no test server with node id: %q", nodeID)

}

func (c *consulServers) stopServer(t *testing.T, addr Addr) {
	c.Lock()
	defer c.Unlock()

	srv := c.servers[addr.String()]
	require.NotNil(t, srv, "no test server for address %s", addr)

	// remove the server so that we don't return it in subsequent discover/watch requests.
	delete(c.servers, addr.String())

	_ = srv.Stop()
}

// startConsulServers starts a multi-server Consul test cluster. It returns a consulServers
// struct containing the servers.
func startConsulServers(t *testing.T, n int, cb testutil.ServerConfigCallback) *consulServers {
	require.Greater(t, n, 0)

	servers := map[string]*testutil.TestServer{}
	for i := 0; i < n; i++ {
		server, err := testutil.NewTestServerConfigT(t, func(c *testutil.TestServerConfig) {
			c.Bootstrap = len(servers) == 0
			for _, srv := range servers {
				addr := fmt.Sprintf("%s:%d", srv.Config.Bind, srv.Config.Ports.SerfLan)
				c.RetryJoin = append(c.RetryJoin, addr)
			}
			c.LogLevel = "warn"
			if cb != nil {
				cb(c)
			}
		})
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = server.Stop()
		})

		addr := mustMakeAddr(t, server.Config.Bind, server.Config.Ports.GRPC)
		servers[addr.String()] = server
	}

	for _, server := range servers {
		server.WaitForLeader(t)
	}
	return &consulServers{servers: servers}
}

func enableACLsConfigFn(c *testutil.TestServerConfig) {
	c.ACL.Enabled = true
	c.ACL.Tokens.InitialManagement = testServerManagementToken
	c.ACL.DefaultPolicy = "deny"
}
