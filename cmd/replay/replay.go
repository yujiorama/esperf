package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielfireman/esperf/esmetrics"
	"github.com/danielfireman/esperf/loadspec"
	"github.com/danielfireman/esperf/metrics"
	"github.com/danielfireman/esperf/reporter"
	"github.com/spf13/cobra"
)

var (
	host          string
	resultsPath   string
	expID         string
	cint          time.Duration
	timeout       time.Duration
	debug         bool
	numClients    int
	isPaused      int32
	continueOn400 bool
)

func init() {
	RootCmd.Flags().StringVar(&host, "mon_host", "", "")
	RootCmd.Flags().DurationVar(&cint, "mon_interval", 5*time.Second, "Interval between metrics collection.")
	RootCmd.Flags().StringVar(&resultsPath, "results_path", "", "")
	RootCmd.Flags().StringVar(&expID, "exp_id", "1", "")
	RootCmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "Timeout to be used in connections to ES.")
	RootCmd.Flags().BoolVar(&debug, "debug", false, "Dump requests and responses.")
	RootCmd.Flags().IntVarP(&numClients, "num_clients", "c", 10, "Number of active clients making requests.")
	RootCmd.Flags().BoolVar(&continueOn400, "continue_on_400s", false, "Whether the loadtest should continue if it receives a 400 response.")
}

var (
	// DefaultLocalAddr is the default local IP address an Attacker uses.
	defaultLocalAddr = net.IPAddr{IP: net.IPv4zero}
	// DefaultConnections is the default amount of max open idle connections per
	// target host.
	defaultConnections = 10000
	r                  runner
)

var RootCmd = &cobra.Command{
	Use:   "replay",
	Short: "Runs a performance testing and collects metrics.",
	Long:  "Multiplatform command line tool to load test and collect metrics from your ElasticSearch deployment.",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if numClients < 1 {
			return fmt.Errorf("number of clients must be positive.")
		}

		var err error
		r = runner{}
		if resultsPath == "" {
			return fmt.Errorf("results path can not be empty. Please set --results_path flag.")
		}

		r.requestsSent = metrics.NewCounter()
		r.errors = metrics.NewCounter()
		r.responseTimes = metrics.NewHistogram()
		r.pauseTimes = metrics.NewHistogram()
		r.clients = make(chan *http.Client, numClients)
		for i := 0; i < numClients; i++ {
			r.clients <- &http.Client{
				Transport: &http.Transport{
					Dial: (&net.Dialer{
						LocalAddr: &net.TCPAddr{IP: defaultLocalAddr.IP, Zone: defaultLocalAddr.Zone},
						KeepAlive: 3 * timeout,
						Timeout:   timeout,
					}).Dial,
					ResponseHeaderTimeout: timeout,
					MaxIdleConnsPerHost:   defaultConnections,
				},
			}
		}

		r.perRequest, err = reporter.NewPerRequestReport(csvFilePath("request", expID, resultsPath))
		if err != nil {
			return err
		}

		// TODO(danielfireman): Review metrics collection design.
		collector, err := esmetrics.NewCollector(host, timeout, debug)
		if err != nil {
			return err
		}
		r.report, err = reporter.New(
			cint,
			timeout,
			reporter.MetricToCSV(r.responseTimes, csvFilePath("response.time", expID, resultsPath)),
			reporter.MetricToCSV(r.pauseTimes, csvFilePath("pause.time", expID, resultsPath)),
			reporter.MetricToCSV(r.requestsSent, csvFilePath("requests.sent", expID, resultsPath)),
			reporter.MetricToCSV(r.errors, csvFilePath("errors", expID, resultsPath)),
			reporter.AddCollector(collector),
			reporter.MetricToCSV(collector.Mem.YoungHeapPool, csvFilePath("mem.young", expID, resultsPath)),
			reporter.MetricToCSV(collector.Mem.TenuredHeapPool, csvFilePath("mem.tenured", expID, resultsPath)),
			reporter.MetricToCSV(collector.Mem.SurvivorHeapPool, csvFilePath("mem.survivor", expID, resultsPath)),
			reporter.MetricToCSV(collector.Mem.Heap, csvFilePath("mem.heap", expID, resultsPath)),
			reporter.MetricToCSV(collector.Mem.NonHeap, csvFilePath("mem.nonheap", expID, resultsPath)),
			reporter.MetricToCSV(collector.Mem.OS, csvFilePath("mem.os", expID, resultsPath)),
			reporter.MetricToCSV(collector.Mem.Swap, csvFilePath("mem.swap", expID, resultsPath)),
			reporter.MetricToCSV(collector.CPU, csvFilePath("cpu", expID, resultsPath)),
			reporter.MetricToCSV(collector.GC.Young, csvFilePath("gc.young", expID, resultsPath)),
			reporter.MetricToCSV(collector.GC.Full, csvFilePath("gc.full", expID, resultsPath)),
		)
		if err != nil {
			return err
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := r.Run(); err != nil {
			return err
		}
		return nil
	},
}

type runner struct {
	clients chan *http.Client
	report  *reporter.Reporter

	requestsSent  *metrics.Counter
	responseTimes *metrics.Histogram
	errors        *metrics.Counter
	pauseTimes    *metrics.Histogram
	perRequest    *reporter.PerRequestReport
}

func csvFilePath(name, expID, resultsPath string) string {
	return filepath.Join(resultsPath, name+"_"+expID+".csv")
}

func (r *runner) Run() error {
	r.report.Start()
	defer r.report.Finish()
	r.perRequest.Start()
	defer r.perRequest.Finish()

	var wg sync.WaitGroup
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, os.Kill)

	// Loading the whole load in memory upfront. This avoid glitches due to disk being slow during high load
	// replays.
	var replayBook []loadspec.Entry
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		entry := loadspec.Entry{}
		if err := json.NewDecoder(strings.NewReader(scanner.Text())).Decode(&entry); err != nil {
			return err
		}
		replayBook = append(replayBook, entry)
	}

	// Note: Having a single worker or a single load generator is a way to guarantee the load will obey to a
	// certain  distribution. For instance, 10 workers generating load following a Poisson distribution is
	// different from having Poisson ruling the overall load impressed on the service.
	// Note 2: Dropping requests made during pauses.
	pauseTime := int64(0)
	pauseChan := make(chan time.Duration)
	for _, entry := range replayBook {
		if pauseTime > 0 {
			pauseTime -= entry.DelaySinceLastNanos
			continue
		} else {
			pauseTime = 0
		}

		start := time.Now()

		// Pretty simple thread-safe pool implementation.
		client := <-r.clients

		// Taking into account the time waiting for a free client.
		delay := entry.DelaySinceLastNanos - (time.Now().Sub(start)).Nanoseconds()
		if delay > 0 {
			time.Sleep(time.Duration(delay))
		}

		wg.Add(1)
		go func(entry loadspec.Entry, client *http.Client) {
			defer wg.Done()
			defer func() {
				r.clients <- client
			}()

			req, err := http.NewRequest(entry.Method, entry.URL, strings.NewReader(entry.Source))
			if err != nil {
				// TODO(danielfireman): Make this more elegant. Leveraging cobra error messages.
				fmt.Printf("Error creating request: %q\n", err)
				os.Exit(-1)
				return
			}
			req.Header.Add("Content-Type", `application/json`)

			if debug {
				dReq, _ := httputil.DumpRequest(req, true)
				fmt.Println(string(dReq))
			}

			r.requestsSent.Inc()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			req.WithContext(ctx)

			resp, err := client.Do(req)
			if err != nil {
				r.errors.Inc()
				fmt.Printf("Error sending request: %q\n", err)
				return
			}

			if debug {
				dResp, _ := httputil.DumpResponse(resp, true)
				fmt.Println(string(dResp))
			}

			defer resp.Body.Close()
			code := resp.StatusCode
			switch {
			default:
				r.errors.Inc()
				r.perRequest.RequestProcessed(time.Now().Unix(), resp.StatusCode, 0, entry.ID)
			case code == http.StatusOK:
				searchResp := struct {
					TookInMillis int64 `json:"took"`
				}{}
				if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
					fmt.Printf("error parsing response: %q\n", err)
					// TODO(danielfireman): Make this more elegant. Leveraging cobra error messages.
					os.Exit(-1)
					return
				}
				r.responseTimes.Record(searchResp.TookInMillis)
				r.perRequest.RequestProcessed(time.Now().Unix(), resp.StatusCode, searchResp.TookInMillis, entry.ID)

			case code >= 400 && code < 500:
				r.perRequest.RequestProcessed(time.Now().Unix(), resp.StatusCode, 0, entry.ID)
				searchResp := struct {
					Error struct {
						Type   string `json:"type"`
						Reason string `json:"reason"`
					} `json:"error"`
				}{}
				if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
					fmt.Printf("error parsing bad request response: %q\n", err)
					// TODO(danielfireman): Make this more elegant. Leveraging cobra error messages.
					os.Exit(-1)
					return
				}
				if !continueOn400 {
					dReq, _ := httputil.DumpRequest(req, true)
					fmt.Printf("error querying server:\nReq:%s\n Error:%+v\n", string(dReq), searchResp.Error)
					// TODO(danielfireman): Make this more elegant. Leveraging cobra error messages.
					os.Exit(-1)
				}
				r.errors.Inc()
			case code == http.StatusServiceUnavailable || code == http.StatusTooManyRequests:
				r.perRequest.RequestProcessed(time.Now().Unix(), resp.StatusCode, 0, entry.ID)
				if atomic.LoadInt32(&isPaused) == 1 {
					return
				}
				ra := resp.Header.Get("Retry-After")
				if ra == "" {
					// TODO(danielfireman): Make this more elegant. Leveraging cobra error messages.
					fmt.Println("Could not extract retry-after information")
					os.Exit(-1)
				}
				pt, err := strconv.ParseFloat(ra, 64)
				if err != nil {
					// TODO(danielfireman): Make this more elegant. Leveraging cobra error messages.
					fmt.Println("Could not extract retry-after information")
					os.Exit(-1)
				}
				pauseMillis := int64(pt * 1e3)
				r.pauseTimes.Record(pauseMillis)
				// If the loadtest is paused, ignore this signal.
				if atomic.LoadInt32(&isPaused) == 1 {
					return
				}
				// Only enqueue if the pause queue is empty.
				if len(pauseChan) == 0 {
					atomic.StoreInt32(&isPaused, 1)
					pauseChan <- time.Duration(pauseMillis) * time.Millisecond
				}
			}
		}(entry, client)

		// Non-blocking check of pauses.
		select {
		case pt := <-pauseChan:
			pauseTime = pt.Nanoseconds()
			time.Sleep(pt)
			atomic.StoreInt32(&isPaused, 0)
		case <-sig:
			fmt.Println("Interrupting load test.")
			return nil
		default:
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	go func() {
		wg.Wait()
		close(pauseChan)
	}()
	// Avoiding any goroutine to be blocked on adding to the pause channel
	for range pauseChan {
	}
	return nil
}
