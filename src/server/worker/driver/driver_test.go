package driver

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"reflect"
	"sync"
	"testing"

	etcd "github.com/coreos/etcd/clientv3"
	embed "github.com/coreos/etcd/embed"
	"github.com/prometheus/client_golang/prometheus"
	prometheus_proto "github.com/prometheus/client_model/go"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/enterprise"
	"github.com/pachyderm/pachyderm/src/client/pkg/require"
	"github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	tu "github.com/pachyderm/pachyderm/src/server/pkg/testutil"
	"github.com/pachyderm/pachyderm/src/server/worker/logs"
)

var etcdClient *etcd.Client
var etcdOnce sync.Once

func getEtcdClient(t *testing.T) *etcd.Client {
	// src/server/pfs/server/driver.go expects an etcd server at "localhost:32379"
	// Try to establish a connection before proceeding with the test (which will
	// fail if the connection can't be established)
	etcdAddress := "localhost:32379"
	etcdOnce.Do(func() {
		require.NoError(t, backoff.Retry(func() error {
			var err error
			etcdClient, err = etcd.New(etcd.Config{
				Endpoints:   []string{etcdAddress},
				DialOptions: client.DefaultDialOptions(),
			})
			if err != nil {
				return fmt.Errorf("could not connect to etcd: %s", err.Error())
			}
			return nil
		}, backoff.NewTestingBackOff()))
	})
	return etcdClient
}

var pachClient *client.APIClient
var pachClientError error
var getPachClientOnce sync.Once

func getPachClient() (*client.APIClient, error) {
	getPachClientOnce.Do(func() {
		if addr := os.Getenv("PACHD_PORT_650_TCP_ADDR"); addr != "" {
			pachClient, pachClientError = client.NewInCluster()
		} else {
			pachClient, pachClientError = client.NewForTest()
		}
	})
	return pachClient, pachClientError
}

var inputRepo = "inputRepo"
var testPipelineInfo = &pps.PipelineInfo{
	Pipeline: client.NewPipeline("testPipeline"),
	Transform: &pps.Transform{
		Cmd: []string{"cp", path.Join("/pfs", inputRepo, "file"), "/pfs/out/file"},
	},
	ParallelismSpec: &pps.ParallelismSpec{
		Constant: 1,
	},
	ResourceRequests: &pps.ResourceSpec{
		Memory: "100M",
		Cpu:    0.5,
	},
	Input: client.NewPFSInput(inputRepo, "/*"),
}

type testEnv struct {
	etcd       *embed.Etcd
	etcdClient *etcd.Client
	mockPachd  *tu.MockPachd
	pachClient *client.APIClient
	driver     *driver
}

func newTestEnv(t *testing.T) *testEnv {
	var err error
	env := &testEnv{}

	// Cleanup any state if we error out early
	defer func() {
		if r := recover(); r != nil {
			env.Close()
			panic(r)
		}
	}()

	tempDirBase := path.Join(os.TempDir(), "pachyderm_test")
	err = os.MkdirAll(tempDirBase, 0700)
	require.NoError(t, err)

	etcdConfig := embed.NewConfig()

	// Create test dirs for etcd data
	etcdConfig.Dir, err = ioutil.TempDir(tempDirBase, "driver_test_etcd_data")
	require.NoError(t, err)
	etcdConfig.WalDir, err = ioutil.TempDir(tempDirBase, "driver_test_etcd_wal")
	require.NoError(t, err)

	// Speed up initial election, hopefully this has no other impact since there
	// is only one etcd instance
	etcdConfig.InitialElectionTickAdvance = true
	etcdConfig.TickMs = 2
	etcdConfig.ElectionMs = 10

	env.etcd, err = embed.StartEtcd(etcdConfig)
	require.NoError(t, err)

	clientUrls := []string{}
	for _, url := range etcdConfig.LCUrls {
		clientUrls = append(clientUrls, url.String())
	}

	env.etcdClient, err = etcd.New(etcd.Config{
		Endpoints:   clientUrls,
		DialOptions: client.DefaultDialOptions(),
	})
	require.NoError(t, err)

	env.mockPachd = tu.NewMockPachd()

	env.pachClient, err = client.NewFromAddress(env.mockPachd.Addr.String())
	require.NoError(t, err)

	// Mock out the enterprise.GetState call that happens during driver construction
	env.mockPachd.Enterprise.GetState.Use(func(context.Context, *enterprise.GetStateRequest) (*enterprise.GetStateResponse, error) {
		return &enterprise.GetStateResponse{State: enterprise.State_NONE}, nil
	})

	var d Driver
	d, err = NewDriver(
		testPipelineInfo,
		env.pachClient,
		NewMockKubeWrapper(),
		env.etcdClient,
		tu.UniqueString("driverTest"),
	)
	env.driver = d.(*driver)
	require.NoError(t, err)

	return env
}

func (env *testEnv) Close() (err error) {
	// We return the first error that occurs during teardown, but still try to close everything
	saveErr := func(e error) {
		if e != nil && err == nil {
			err = e
		}
	}

	if env.pachClient != nil {
		saveErr(env.pachClient.Close())
	}

	if env.mockPachd != nil {
		saveErr(env.mockPachd.Close())
	}

	if env.etcdClient != nil {
		saveErr(env.etcdClient.Close())
	}

	if env.etcd != nil {
		env.etcd.Close()
	}

	return err
}

func requireLogs(t *testing.T, pattern string, cb func(logs.TaggedLogger)) {
	logger := logs.NewMockLogger()
	buffer := &bytes.Buffer{}
	logger.Writer = buffer
	logger.Job = "job-id"

	cb(logger)

	result := string(buffer.Bytes())

	if pattern == "" {
		require.Equal(t, "", result, "callback should not have logged anything")
	} else {
		require.Matches(t, pattern, result, "callback did not log the expected message")
	}
}

func requireMetric(t *testing.T, metric prometheus.Collector, labels []string, cb func(prometheus_proto.Metric)) {
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(metric))

	stats, err := reg.Gather()
	require.NoError(t, err)

	// Add a placeholder for the state label even if it isn't used
	for len(labels) < 3 {
		labels = append(labels, "")
	}

	// We only have one metric in the registry, so skip over the family level
	for _, family := range stats {
		for _, metric := range family.Metric {
			var pipeline, job, state string
			for _, pair := range metric.Label {
				switch *pair.Name {
				case "pipeline":
					pipeline = *pair.Value
				case "job":
					job = *pair.Value
				case "state":
					state = *pair.Value
				default:
					require.True(t, false, fmt.Sprintf("unexpected metric label: %s", *pair.Name))
				}
			}

			metricLabels := []string{pipeline, job, state}
			if reflect.DeepEqual(labels, metricLabels) {
				cb(*metric)
				return
			}
		}
	}

	require.True(t, false, fmt.Sprintf("no matching metric found for labels: %v", labels))
}

func requireCounter(t *testing.T, counter *prometheus.CounterVec, labels []string, value float64) {
	requireMetric(t, counter, labels, func(m prometheus_proto.Metric) {
		require.NotNil(t, m.Counter)
		require.Equal(t, value, *m.Counter.Value)
	})
}

func requireHistogram(t *testing.T, histogram *prometheus.HistogramVec, labels []string, value uint64) {
	requireMetric(t, histogram, labels, func(m prometheus_proto.Metric) {
		require.NotNil(t, m.Histogram)
		require.Equal(t, value, *m.Histogram.SampleCount)
	})
}

func TestUpdateCounter(t *testing.T) {
	env := newTestEnv(t)
	defer env.Close()

	env.driver.pipelineInfo.ID = "foo"

	counterVec := prometheus.NewCounterVec(
		prometheus.CounterOpts{Namespace: "test", Subsystem: "driver", Name: "counter"},
		[]string{"pipeline", "job"},
	)

	counterVecWithState := prometheus.NewCounterVec(
		prometheus.CounterOpts{Namespace: "test", Subsystem: "driver", Name: "counter_with_state"},
		[]string{"pipeline", "job", "state"},
	)

	// Passing a state to the stateless counter should error
	requireLogs(t, "expected 2 label values but got 3", func(logger logs.TaggedLogger) {
		env.driver.updateCounter(counterVec, logger, "bar", func(c prometheus.Counter) {
			require.True(t, false, "should have errored")
		})
	})

	// updateCounter should pass a valid counter with the selected tags
	requireLogs(t, "", func(logger logs.TaggedLogger) {
		env.driver.updateCounter(counterVec, logger, "", func(c prometheus.Counter) {
			c.Add(1)
		})
	})

	// Check that the counter was incremented
	requireCounter(t, counterVec, []string{"foo", "job-id"}, 1)

	// Not passing a state to the stateful counter should error
	requireLogs(t, "expected 3 label values but got 2", func(logger logs.TaggedLogger) {
		env.driver.updateCounter(counterVecWithState, logger, "", func(c prometheus.Counter) {
			require.True(t, false, "should have errored")
		})
	})

	// updateCounter should pass a valid counter with the selected tags
	requireLogs(t, "", func(logger logs.TaggedLogger) {
		env.driver.updateCounter(counterVecWithState, logger, "bar", func(c prometheus.Counter) {
			c.Add(1)
		})
	})

	// Check that the counter was incremented
	requireCounter(t, counterVecWithState, []string{"foo", "job-id", "bar"}, 1)
}

func TestUpdateHistogram(t *testing.T) {
	env := newTestEnv(t)
	defer env.Close()

	env.driver.pipelineInfo.ID = "foo"

	histogramVec := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "test", Subsystem: "driver", Name: "histogram",
			Buckets: prometheus.ExponentialBuckets(1.0, 2.0, 20),
		},
		[]string{"pipeline", "job"},
	)

	histogramVecWithState := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "test", Subsystem: "driver", Name: "histogram_with_state",
			Buckets: prometheus.ExponentialBuckets(1.0, 2.0, 20),
		},
		[]string{"pipeline", "job", "state"},
	)

	// Passing a state to the stateless histogram should error
	requireLogs(t, "expected 2 label values but got 3", func(logger logs.TaggedLogger) {
		env.driver.updateHistogram(histogramVec, logger, "bar", func(h prometheus.Observer) {
			require.True(t, false, "should have errored")
		})
	})

	requireLogs(t, "", func(logger logs.TaggedLogger) {
		env.driver.updateHistogram(histogramVec, logger, "", func(h prometheus.Observer) {
			h.Observe(0)
		})
	})

	// Check that the counter was incremented
	requireHistogram(t, histogramVec, []string{"foo", "job-id"}, 1)

	// Not passing a state to the stateful histogram should error
	requireLogs(t, "expected 3 label values but got 2", func(logger logs.TaggedLogger) {
		env.driver.updateHistogram(histogramVecWithState, logger, "", func(h prometheus.Observer) {
			require.True(t, false, "should have errored")
		})
	})

	requireLogs(t, "", func(logger logs.TaggedLogger) {
		env.driver.updateHistogram(histogramVecWithState, logger, "bar", func(h prometheus.Observer) {
			h.Observe(0)
		})
	})

	// Check that the counter was incremented
	requireHistogram(t, histogramVecWithState, []string{"foo", "job-id", "bar"}, 1)
}

/*
	ctx context.Context,
	data []*common.Input,
	inputTree *hashtree.Ordered,
	logger logs.TaggedLogger,
	cb func(*pps.ProcessStats) error,
*/

func provisionPipeline(d *driver) {
}

func TestWithData(t *testing.T) {
}

func TestWithDataCancel(t *testing.T) {
}

func TestWithDataGit(t *testing.T) {
}

func TestRunUserCode(t *testing.T) {
}

func TestRunUserErrorHandlingCode(t *testing.T) {
}

func TestUpdateJobState(t *testing.T) {
}

func TestDeleteJob(t *testing.T) {
}

/*
func lookupDockerUser(userArg string) (_ *user.User, retErr error) {
func lookupGroup(group string) (_ *user.Group, retErr error) {
func (d *driver) WithCtx(ctx context.Context) Driver {
func (d *driver) Jobs() col.Collection {
func (d *driver) Pipelines() col.Collection {
func (d *driver) Plans() col.Collection {
func (d *driver) Shards() col.Collection {
func (d *driver) Chunks(jobID string) col.Collection {
func (d *driver) Merges(jobID string) col.Collection {

func (d *driver) WithData(
func (d *driver) downloadData(
func (d *driver) downloadGitData(pachClient *client.APIClient, dir string, input *common.Input) error {
func (d *driver) linkData(inputs []*common.Input, dir string) error {
func (d *driver) unlinkData(inputs []*common.Input) error {

func (d *driver) RunUserCode(ctx context.Context, logger logs.TaggedLogger, environ []string, procStats *pps.ProcessStats, rawDatumTimeout *types.Duration) (retErr error) {
func (d *driver) RunUserErrorHandlingCode(ctx context.Context, logger logs.TaggedLogger, environ []string, procStats *pps.ProcessStats, rawDatumTimeout *types.Duration) (retErr error) {

func (d *driver) UpdateJobState(ctx context.Context, jobID string, statsCommit *pfs.Commit, state pps.JobState, reason string) error {
func (d *driver) DeleteJob(stm col.STM, jobPtr *pps.EtcdJobInfo) error {

func (d *driver) updateCounter(
func (d *driver) updateHistogram(
func (d *driver) reportUserCodeStats(logger logs.TaggedLogger) {
func (d *driver) reportDeferredUserCodeStats(err error, start time.Time, procStats *pps.ProcessStats, logger logs.TaggedLogger) {
func (d *driver) ReportUploadStats(start time.Time, procStats *pps.ProcessStats, logger logs.TaggedLogger) {
func (d *driver) reportDownloadSizeStats(downSize float64, logger logs.TaggedLogger) {
func (d *driver) reportDownloadTimeStats(start time.Time, procStats *pps.ProcessStats, logger logs.TaggedLogger) {
*/