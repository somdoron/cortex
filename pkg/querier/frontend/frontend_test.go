package frontend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	strconv "strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/gorilla/mux"
	otgrpc "github.com/opentracing-contrib/go-grpc"
	"github.com/opentracing-contrib/go-stdlib/nethttp"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	jaeger "github.com/uber/jaeger-client-go"
	"github.com/uber/jaeger-client-go/config"
	"github.com/weaveworks/common/httpgrpc"
	httpgrpc_server "github.com/weaveworks/common/httpgrpc/server"
	"github.com/weaveworks/common/middleware"
	"github.com/weaveworks/common/user"
	"go.uber.org/atomic"
	"google.golang.org/grpc"

	"github.com/cortexproject/cortex/pkg/querier"
	"github.com/cortexproject/cortex/pkg/util/flagext"
	"github.com/cortexproject/cortex/pkg/util/services"
)

const (
	query        = "/api/v1/query_range?end=1536716898&query=sum%28container_memory_rss%29+by+%28namespace%29&start=1536673680&step=120"
	responseBody = `{"status":"success","data":{"resultType":"Matrix","result":[{"metric":{"foo":"bar"},"values":[[1536673680,"137"],[1536673780,"137"]]}]}}`
)

func TestFrontend(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte("Hello World"))
		require.NoError(t, err)
	})
	test := func(addr string) {
		req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/", addr), nil)
		require.NoError(t, err)
		err = user.InjectOrgIDIntoHTTPRequest(user.InjectOrgID(context.Background(), "1"), req)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode)

		body, err := ioutil.ReadAll(resp.Body)
		require.NoError(t, err)

		assert.Equal(t, "Hello World", string(body))
	}

	testFrontend(t, defaultFrontendConfig(), handler, test, false, nil)
	testFrontend(t, defaultFrontendConfig(), handler, test, true, nil)
}

func TestFrontendPropagateTrace(t *testing.T) {
	closer, err := config.Configuration{}.InitGlobalTracer("test")
	require.NoError(t, err)
	defer closer.Close()

	observedTraceID := make(chan string, 2)

	handler := middleware.Tracer{}.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sp := opentracing.SpanFromContext(r.Context())
		defer sp.Finish()

		traceID := fmt.Sprintf("%v", sp.Context().(jaeger.SpanContext).TraceID())
		observedTraceID <- traceID

		_, err = w.Write([]byte(responseBody))
		require.NoError(t, err)
	}))

	test := func(addr string) {
		sp, ctx := opentracing.StartSpanFromContext(context.Background(), "client")
		defer sp.Finish()
		traceID := fmt.Sprintf("%v", sp.Context().(jaeger.SpanContext).TraceID())

		req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/%s", addr, query), nil)
		require.NoError(t, err)
		req = req.WithContext(ctx)
		err = user.InjectOrgIDIntoHTTPRequest(user.InjectOrgID(ctx, "1"), req)
		require.NoError(t, err)

		req, tr := nethttp.TraceRequest(opentracing.GlobalTracer(), req)
		defer tr.Finish()

		client := http.Client{
			Transport: &nethttp.Transport{},
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode)

		defer resp.Body.Close()
		_, err = ioutil.ReadAll(resp.Body)
		require.NoError(t, err)

		// Query should do one call.
		assert.Equal(t, traceID, <-observedTraceID)
	}
	testFrontend(t, defaultFrontendConfig(), handler, test, false, nil)
	testFrontend(t, defaultFrontendConfig(), handler, test, true, nil)
}

func TestFrontend_RequestHostHeaderWhenDownstreamURLIsConfigured(t *testing.T) {
	// Create an HTTP server listening locally. This server mocks the downstream
	// Prometheus API-compatible server.
	downstreamListen, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	observedHost := make(chan string, 2)
	downstreamServer := http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			observedHost <- r.Host

			_, err := w.Write([]byte(responseBody))
			require.NoError(t, err)
		}),
	}

	defer downstreamServer.Shutdown(context.Background()) //nolint:errcheck
	go downstreamServer.Serve(downstreamListen)           //nolint:errcheck

	// Configure the query-frontend with the mocked downstream server.
	config := defaultFrontendConfig()
	config.DownstreamURL = fmt.Sprintf("http://%s", downstreamListen.Addr())

	// Configure the test to send a request to the query-frontend and assert on the
	// Host HTTP header received by the downstream server.
	test := func(addr string) {
		req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/%s", addr, query), nil)
		require.NoError(t, err)

		ctx := context.Background()
		req = req.WithContext(ctx)
		err = user.InjectOrgIDIntoHTTPRequest(user.InjectOrgID(ctx, "1"), req)
		require.NoError(t, err)

		client := http.Client{
			Transport: &nethttp.Transport{},
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode)

		defer resp.Body.Close()
		_, err = ioutil.ReadAll(resp.Body)
		require.NoError(t, err)

		// We expect the Host received by the downstream is the downstream host itself
		// and not the query-frontend host.
		downstreamReqHost := <-observedHost
		assert.Equal(t, downstreamListen.Addr().String(), downstreamReqHost)
		assert.NotEqual(t, downstreamReqHost, addr)
	}

	testFrontend(t, config, nil, test, false, nil)
	testFrontend(t, config, nil, test, true, nil)
}

// TestFrontendCancel ensures that when client requests are cancelled,
// the underlying query is correctly cancelled _and not retried_.
func TestFrontendCancel(t *testing.T) {
	var tries atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		tries.Inc()
	})
	test := func(addr string) {
		req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/", addr), nil)
		require.NoError(t, err)
		err = user.InjectOrgIDIntoHTTPRequest(user.InjectOrgID(context.Background(), "1"), req)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		req = req.WithContext(ctx)

		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()

		_, err = http.DefaultClient.Do(req)
		require.Error(t, err)

		time.Sleep(100 * time.Millisecond)
		assert.Equal(t, int32(1), tries.Load())
	}
	testFrontend(t, defaultFrontendConfig(), handler, test, false, nil)
	tries.Store(0)
	testFrontend(t, defaultFrontendConfig(), handler, test, true, nil)
}

func TestFrontendCancelStatusCode(t *testing.T) {
	for _, test := range []struct {
		status int
		err    error
	}{
		{http.StatusInternalServerError, errors.New("unknown")},
		{http.StatusGatewayTimeout, context.DeadlineExceeded},
		{StatusClientClosedRequest, context.Canceled},
		{http.StatusBadRequest, httpgrpc.Errorf(http.StatusBadRequest, "")},
	} {
		t.Run(test.err.Error(), func(t *testing.T) {
			w := httptest.NewRecorder()
			writeError(w, test.err)
			require.Equal(t, test.status, w.Result().StatusCode)
		})
	}
}

func TestFrontendCheckReady(t *testing.T) {
	for _, tt := range []struct {
		name             string
		connectedClients int32
		msg              string
		readyForRequests bool
	}{
		{"connected clients are ready", 3, "", true},
		{"no url, no clients is not ready", 0, "not ready: number of queriers connected to query-frontend is 0", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &Frontend{
				connectedClients: atomic.NewInt32(tt.connectedClients),
				log:              log.NewNopLogger(),
			}
			err := f.CheckReady(context.Background())
			errMsg := ""

			if err != nil {
				errMsg = err.Error()
			}

			require.Equal(t, tt.msg, errMsg)
		})
	}
}

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (sb *syncBuf) Write(p []byte) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	return sb.buf.Write(p)
}

func (sb *syncBuf) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	return sb.buf.String()
}

func TestFrontend_LogsSlowQueriesFormValues(t *testing.T) {
	// Create an HTTP server listening locally. This server mocks the downstream
	// Prometheus API-compatible server.
	downstreamListen, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	downstreamServer := http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := w.Write([]byte(responseBody))
			require.NoError(t, err)
		}),
	}

	defer downstreamServer.Shutdown(context.Background()) //nolint:errcheck
	go downstreamServer.Serve(downstreamListen)           //nolint:errcheck

	// Configure the query-frontend with the mocked downstream server.
	config := defaultFrontendConfig()
	config.Handler.LogQueriesLongerThan = 1 * time.Microsecond
	config.DownstreamURL = fmt.Sprintf("http://%s", downstreamListen.Addr())

	var buf syncBuf
	l := log.NewLogfmtLogger(&buf)

	test := func(addr string) {
		data := url.Values{}
		data.Set("test", "form")
		data.Set("issue", "3111")

		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/?foo=bar", addr), strings.NewReader(data.Encode()))
		req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Add("Content-Length", strconv.Itoa(len(data.Encode())))

		ctx := context.Background()
		req = req.WithContext(ctx)
		assert.NoError(t, err)
		err = user.InjectOrgIDIntoHTTPRequest(user.InjectOrgID(ctx, "1"), req)
		assert.NoError(t, err)

		client := http.Client{
			Transport: &nethttp.Transport{},
		}

		resp, err := client.Do(req)
		assert.NoError(t, err)
		b, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		assert.NoError(t, err)
		assert.Equal(t, 200, resp.StatusCode, string(b))

		logs := buf.String()
		assert.NotContains(t, logs, "unable to parse form for request")
		assert.Contains(t, logs, "msg=\"slow query detected\"")
		assert.Contains(t, logs, "param_issue=3111")
		assert.Contains(t, logs, "param_test=form")
		assert.Contains(t, logs, "param_foo=bar")
	}

	testFrontend(t, config, nil, test, false, l)
}

func TestFrontend_ReturnsRequestBodyTooLargeError(t *testing.T) {
	// Create an HTTP server listening locally. This server mocks the downstream
	// Prometheus API-compatible server.
	downstreamListen, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	downstreamServer := http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := w.Write([]byte(responseBody))
			require.NoError(t, err)
		}),
	}

	defer downstreamServer.Shutdown(context.Background()) //nolint:errcheck
	go downstreamServer.Serve(downstreamListen)           //nolint:errcheck

	// Configure the query-frontend with the mocked downstream server.
	config := defaultFrontendConfig()
	config.DownstreamURL = fmt.Sprintf("http://%s", downstreamListen.Addr())
	config.Handler.MaxBodySize = 1

	test := func(addr string) {
		data := url.Values{}
		data.Set("test", "max body size")

		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/?foo=bar", addr), strings.NewReader(data.Encode()))
		req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Add("Content-Length", strconv.Itoa(len(data.Encode())))

		ctx := context.Background()
		req = req.WithContext(ctx)
		assert.NoError(t, err)
		err = user.InjectOrgIDIntoHTTPRequest(user.InjectOrgID(ctx, "1"), req)
		assert.NoError(t, err)

		client := http.Client{
			Transport: &nethttp.Transport{},
		}

		resp, err := client.Do(req)
		assert.NoError(t, err)
		b, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		assert.NoError(t, err)

		assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, string(b))
	}

	testFrontend(t, config, nil, test, false, nil)
}

func testFrontend(t *testing.T, config CombinedFrontendConfig, handler http.Handler, test func(addr string), matchMaxConcurrency bool, l log.Logger) {
	logger := log.NewNopLogger()
	if l != nil {
		logger = l
	}

	var (
		workerConfig  WorkerConfig
		querierConfig querier.Config
	)
	flagext.DefaultValues(&workerConfig)
	workerConfig.Parallelism = 1
	workerConfig.MatchMaxConcurrency = matchMaxConcurrency
	querierConfig.MaxConcurrent = 1

	// localhost:0 prevents firewall warnings on Mac OS X.
	grpcListen, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	workerConfig.FrontendAddress = grpcListen.Addr().String()

	httpListen, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	rt, v1, v2, err := InitFrontend(config, limits{}, 0, logger, nil)
	require.NoError(t, err)
	require.NotNil(t, rt)
	// v1 will be nil if DownstreamURL is defined.
	require.Nil(t, v2)
	if v1 != nil {
		defer v1.Close()
	}

	grpcServer := grpc.NewServer(
		grpc.StreamInterceptor(otgrpc.OpenTracingStreamServerInterceptor(opentracing.GlobalTracer())),
	)
	defer grpcServer.GracefulStop()

	if v1 != nil {
		RegisterFrontendServer(grpcServer, v1)
	}

	r := mux.NewRouter()
	r.PathPrefix("/").Handler(middleware.Merge(
		middleware.AuthenticateUser,
		middleware.Tracer{},
	).Wrap(NewHandler(config.Handler, rt, logger)))

	httpServer := http.Server{
		Handler: r,
	}
	defer httpServer.Shutdown(context.Background()) //nolint:errcheck

	go httpServer.Serve(httpListen) //nolint:errcheck
	go grpcServer.Serve(grpcListen) //nolint:errcheck

	var worker services.Service
	worker, err = NewWorker(workerConfig, querierConfig, httpgrpc_server.NewServer(handler), logger)
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), worker))

	test(httpListen.Addr().String())

	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), worker))
}

func defaultFrontendConfig() CombinedFrontendConfig {
	config := CombinedFrontendConfig{}
	flagext.DefaultValues(&config)
	flagext.DefaultValues(&config.Handler)
	flagext.DefaultValues(&config.FrontendV1)
	flagext.DefaultValues(&config.FrontendV2)
	return config
}

type limits struct {
	queriers int
}

func (l limits) MaxQueriersPerUser(_ string) int {
	return l.queriers
}
