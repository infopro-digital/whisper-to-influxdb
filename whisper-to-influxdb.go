package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/influxdata/influxdb/client/v2"
	"github.com/kisielk/whisper-go/whisper"
	"github.com/rcrowley/go-metrics"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var whisperDir string // will contain whisper directory, with no / at the end
var influxWorkers, whisperWorkers int
var from, until uint
var fromTime, untilTime uint32
var influxWorkersWg, whisperWorkersWg sync.WaitGroup
var whisperFiles chan string
var foundFiles chan string
var finishedFiles chan string
var influxSeries chan *abstractSerie

var influxHost, influxUser, influxPass, influxDb string
var influxPort uint

var influxClient client.Client

var whisperReadTimer metrics.Timer
var influxWriteTimer metrics.Timer

var skipUntil string
var skipCounter uint64

var influxPrefix string
var include, exclude string
var verbose bool
var all bool
var skipInfluxErrors bool
var skipWhisperErrors bool

var statsInterval uint
var exit chan int

type influxError struct {
	Error string `json:"error"`
}

func seriesString(bp client.BatchPoints) string {
	return fmt.Sprintf("InfluxDB series (%d points)", len(bp.Points()))
}

// needed to keep track of what's the next file in line that needs processing
// because the workers can finish out of order, relative to the order
// of the filesystem walk which uses inode order.
// this ensures if you use skipUntil, it resumes from the right pos, without forgetting any
// other files that also needed processing.
func keepOrder() {
	type inProgress struct {
		Path string
		Next *inProgress
	}
	var firstInProgress *inProgress
	// we keep a list, the InProgress list, like so : A-B-C-D-E-F
	// the order of that list, is the inode/filesystem order

	for {
		select {
		case found := <-foundFiles:
			// add item to end of linked list
			i := &inProgress{
				Path: found,
			}
			if firstInProgress == nil {
				firstInProgress = i
			} else {
				var cur *inProgress
				for cur = firstInProgress; cur.Next != nil; cur = cur.Next {
				}
				cur.Next = i
			}
		case finished := <-finishedFiles:
			// firstInProgress will always be non-nil, because the file must have been added before it finished
			// in the list above,
			// when B completes, strip it out of the list (link to A to C)
			// when A completes, delete A, update first to point to B
			var prev *inProgress
			for cur := firstInProgress; cur != nil; cur = cur.Next {
				if cur.Path == finished {
					if prev != nil {
						prev.Next = cur.Next
					} else {
						firstInProgress = cur.Next
					}
					break
				}
				prev = cur
			}
		case code := <-exit:
			if firstInProgress != nil {
				fmt.Println("the next file that needed processing was", firstInProgress.Path, "you can resume from there")
			}
			os.Exit(code)
		}
	}
}

var haproxyRegexp = regexp.MustCompile(`^\[proxy_name=(.+),service_name=(.+)$`)

func transformHaproxyPoint(measurement string, rawval float64, fields map[string]interface{}) (bool, string) {
	measureKey := ""
	switch measurement {
	case "bytes_in":
		measureKey = "bin"
		fields[measureKey] = int64(rawval)
	case "bytes_out":
		measureKey = "bout"
		fields[measureKey] = int64(rawval)
	case "cli_abrt":
		measureKey = "cli_abort"
		fields[measureKey] = int64(rawval)
	case "connect_time_avg":
		measureKey = "ctime"
		fields[measureKey] = int64(rawval)
	case "denied_request":
		measureKey = "dreq"
		fields[measureKey] = int64(rawval)
	case "denied_response":
		measureKey = "dresp"
		fields[measureKey] = int64(rawval)
	case "error_connection":
		measureKey = "econ"
		fields[measureKey] = int64(rawval)
	case "error_request":
		measureKey = "ereq"
		fields[measureKey] = int64(rawval)
	case "error_response":
		measureKey = "eresp"
		fields[measureKey] = int64(rawval)
	case "session_rate":
		measureKey = "rate"
		fields[measureKey] = int64(rawval)
	case "request_rate":
		measureKey = "req_rate"
		fields[measureKey] = int64(rawval)
	case "response_1xx":
		measureKey = "http_response.1xx"
		fields[measureKey] = int64(rawval)
	case "response_2xx":
		measureKey = "http_response.2xx"
		fields[measureKey] = int64(rawval)
	case "response_3xx":
		measureKey = "http_response.3xx"
		fields[measureKey] = int64(rawval)
	case "response_4xx":
		measureKey = "http_response.4xx"
		fields[measureKey] = int64(rawval)
	case "response_5xx":
		measureKey = "http_response.5xx"
		fields[measureKey] = int64(rawval)
	case "response_other":
		measureKey = "http_response.other"
		fields[measureKey] = int64(rawval)
	case "queue_time_avg":
		measureKey = "qtime"
		fields[measureKey] = int64(rawval)
	case "queue_current":
		measureKey = "qcur"
		fields[measureKey] = int64(rawval)
	case "redistributed":
		measureKey = "wredis"
		fields[measureKey] = int64(rawval)
	case "retries":
		measureKey = "wret"
		fields[measureKey] = int64(rawval)
	case "response_time_avg":
		measureKey = "rtime"
		fields[measureKey] = int64(rawval)
	case "session_current":
		measureKey = "scur"
		fields[measureKey] = int64(rawval)
	case "session_total":
		measureKey = "stot"
		fields[measureKey] = int64(rawval)
	case "srv_abrt":
		measureKey = "srv_abort"
		fields[measureKey] = int64(rawval)
	case "comp_rsp":
		measureKey = measurement
		fields[measureKey] = int64(rawval)
	case "comp_byp":
		measureKey = measurement
		fields[measureKey] = int64(rawval)
	case "comp_in":
		measureKey = measurement
		fields[measureKey] = int64(rawval)
	case "comp_out":
		measureKey = measurement
		fields[measureKey] = int64(rawval)
	case "downtime":
		measureKey = measurement
		fields[measureKey] = int64(rawval)
	case "req_tot":
		measureKey = measurement
		fields[measureKey] = int64(rawval)
	default:
		log.Printf("Unhandled haproxy metric: %s, dropping\n", measurement)
		return false, ""
	}

	return true, measureKey
}

func transformElasticsearchPoint(measurement string, rawval float64,
	fields map[string]interface{}) (bool, string, string) {
	measureKey := ""
	influxMeasurement := ""
	switch measurement {
	case "docs-count":
		measureKey = "docs_count"
		fields[measureKey] = rawval
		influxMeasurement = "elasticsearch_clusterstats_indices"
	case "docs-deleted":
		measureKey = "docs_deleted"
		fields[measureKey] = rawval
		influxMeasurement = "elasticsearch_clusterstats_indices"
	case "_shards-total":
		measureKey = "shards_total"
		fields[measureKey] = rawval
		influxMeasurement = "elasticsearch_clusterstats_indices"
	case "store-size_in_bytes":
		measureKey = "store_size_in_bytes"
		fields[measureKey] = rawval
		influxMeasurement = "elasticsearch_clusterstats_indices"

	case "search-fetch_time_in_millis":
		measureKey = "search_fetch_time_in_millis"
		fields[measureKey] = rawval
		influxMeasurement = "elasticsearch_indices"
	case "search-fetch_total":
		measureKey = "search_fetch_total"
		fields[measureKey] = rawval
		influxMeasurement = "elasticsearch_indices"
	case "search-query_time_in_millis":
		measureKey = "search_query_time_in_millis"
		fields[measureKey] = rawval
		influxMeasurement = "elasticsearch_indices"
	case "search-query_total":
		measureKey = "search_query_total"
		fields[measureKey] = rawval
		influxMeasurement = "elasticsearch_indices"
	case "search-scroll_time_in_millis":
		measureKey = "search_scroll_time_in_millis"
		fields[measureKey] = rawval
		influxMeasurement = "elasticsearch_indices"
	case "search-scroll_total":
		measureKey = "search_scroll_total"
		fields[measureKey] = rawval
		influxMeasurement = "elasticsearch_indices"

	case "active_shards":
		measureKey = "active_shards"
		fields[measureKey] = int64(rawval)
		influxMeasurement = "elasticsearch_cluster_health"
	case "active_primary_shards":
		measureKey = "active_primary_shards"
		fields[measureKey] = int64(rawval)
		influxMeasurement = "elasticsearch_cluster_health"
	case "initializing_shards":
		measureKey = "initializing_shards"
		fields[measureKey] = int64(rawval)
		influxMeasurement = "elasticsearch_cluster_health"
	case "number_of_nodes":
		measureKey = "number_of_nodes"
		fields[measureKey] = int64(rawval)
		influxMeasurement = "elasticsearch_cluster_health"
	case "relocating_shards":
		measureKey = "relocating_shards"
		fields[measureKey] = int64(rawval)
		influxMeasurement = "elasticsearch_cluster_health"
	case "unassigned_shards":
		measureKey = "unassigned_shards"
		fields[measureKey] = int64(rawval)
		influxMeasurement = "elasticsearch_cluster_health"
	case "number_of_data_nodes":
		measureKey = "number_of_data_nodes"
		fields[measureKey] = int64(rawval)
		influxMeasurement = "elasticsearch_cluster_health"

	// Not found in telegraf metrics
	case "_shards-failed":
		return false, "", ""
	// Not found in telegraf metrics
	case "_shards-successful":
		return false, "", ""
	default:
		fmt.Printf("TODO: %v\n", measurement)
		return false, "", ""
	}
	return true, measureKey, influxMeasurement
}

func transformApachePoint(measureCategory string, measureField string, rawval float64,
	fields map[string]interface{}) (bool, string) {
	measureKey := ""
	switch measureCategory {
	case "":
		switch measureField {
		case "apache_connections":
			measureKey = "BusyWorkers"
			fields[measureKey] = rawval
		case "apache_bytes":
			measureKey = "TotalkBytes"
			fields[measureKey] = rawval
		case "apache_idle_workers":
			measureKey = "IdleWorkers"
			fields[measureKey] = rawval
		case "apache_requests": // not sure
			measureKey = "TotalAccesses"
			fields[measureKey] = rawval
		default:
			return false, ""
		}
	case "apache_scoreboard":
		switch measureField {
		case "closing":
			measureKey = "scboard_closing"
			fields[measureKey] = rawval
		case "dnslookup":
			measureKey = "scboard_dnslookup"
			fields[measureKey] = rawval
		case "finishing":
			measureKey = "scboard_finishing"
			fields[measureKey] = rawval
		case "idle_cleanup":
			measureKey = "scboard_idle_cleanup"
			fields[measureKey] = rawval
		case "keepalive":
			measureKey = "scboard_keepalive"
			fields[measureKey] = rawval
		case "logging":
			measureKey = "scboard_logging"
			fields[measureKey] = rawval
		case "open":
			measureKey = "scboard_open"
			fields[measureKey] = rawval
		case "reading":
			measureKey = "scboard_reading"
			fields[measureKey] = rawval
		case "sending":
			measureKey = "scboard_sending"
			fields[measureKey] = rawval
		case "starting":
			measureKey = "scboard_starting"
			fields[measureKey] = rawval
		case "waiting":
			measureKey = "scboard_waiting"
			fields[measureKey] = rawval
		default:
			return false, ""
		}
	default:
		return false, ""
	}
	return true, measureKey
}

func transformWhisperPointToInfluxPoint(whisperPoint whisper.Point, measureName string, measureKey string,
	measureSplited []string, measureSplitedLen int) *client.Point {
	tags := map[string]string{
		// Host are encoded with _, replace with .
		"host": strings.Replace(measureSplited[0], "_", ".", -1),
	}

	fields := map[string]interface{}{}

	if measureSplitedLen >= 2 {
		if measureSplitedLen == 5 {
			switch measureSplited[1] {
			case "apache":
				if measureSplited[2] != "apache" {
					log.Printf("Unhandled apache metric: %s, dropping\n", measureSplited[2])
					return nil
				}
				ok, key := transformApachePoint(measureSplited[3], measureSplited[4], whisperPoint.Value, fields)
				if !ok {
					fmt.Printf("TODO: %v\n", measureSplited)
					return nil
				}

				measureKey = key
			case "curl_json":
				switch measureSplited[2] {
				case "elasticsearch":
					ok, key, mn := transformElasticsearchPoint(measureSplited[4], whisperPoint.Value, fields)
					if !ok {
						return nil
					}

					measureKey = key
					measureName = mn
				default:
					log.Printf("Unhandled curl_json metric: %s, dropping\n", measureSplited[2])
					return nil
				}
			case "haproxy":
				ok, key := transformHaproxyPoint(measureSplited[4], whisperPoint.Value, fields)
				if !ok {
					return nil
				}

				measureKey = key

				rpResults := haproxyRegexp.FindStringSubmatch(measureSplited[2])
				if len(rpResults) != 3 {
					log.Printf("Unexpected result while matching haproxy. (%d != 3): %s\n",
						len(rpResults),
						measureSplited[2],
					)
					return nil
				}

				// The regexp is not perfect i know...
				tags["proxy"] = rpResults[1]
				tags["type"] = strings.Replace(strings.ToLower(rpResults[2]), "]", "", -1)
			}

			switch measureSplited[3] {
			case "cpu":
				measureKey = measureSplited[4]
				tags["cpu"] = fmt.Sprintf("cpu%s", measureSplited[2])
				fields[measureKey] = whisperPoint.Value
			}
		} else if measureSplitedLen == 4 {
			switch measureSplited[1] {
			case "apache":
				if measureSplited[2] != "apache" {
					log.Printf("Unhandled apache metric: %s, dropping\n", measureSplited[2])
					return nil
				}
				ok, key := transformApachePoint("", measureSplited[3], whisperPoint.Value, fields)
				if !ok {
					fmt.Printf("TODO: %v\n", measureSplited)
					return nil
				}

				measureKey = key
			case "load":
				if measureSplited[2] != "load" {
					log.Printf("Unhandled load metric: %s, dropping\n", measureSplited[2])
					return nil
				}

				switch measureSplited[3] {
				case "shortterm":
					measureKey = "load1"
				case "midterm":
					measureKey = "load5"
				case "longterm":
					measureKey = "load15"
				default:
					log.Printf("Unhandled load metric point: %s, dropping\n", measureSplited[3])
					return nil
				}

				measureName = "system"
				fields[measureKey] = whisperPoint.Value
			default:
				fmt.Printf("TODO: %v\n", measureSplited)
				return nil
			}
		} else if measureSplitedLen == 3 {
			switch measureSplited[1] {
			case "uptime":
				if measureSplited[2] != "uptime" {
					log.Printf("Unhandled uptime metric: %s, dropping\n", measureSplited[2])
					return nil
				}

				measureName = "system"
				fields[measureKey] = int64(whisperPoint.Value)
			default:
				fmt.Printf("TODO: %v\n", measureSplited)
				return nil
			}
		} else {
			switch measureSplited[1] {
			case "haproxy":
				// Ignore those metrics, they are already more precise in each frontend/backend
				return nil
			}
		}
	} else {
		log.Printf("Unknown metric %v, ignoring.\n", measureSplited)
		return nil
	}

	p, _ := client.NewPoint(measureName, tags, fields,
		time.Unix(int64(whisperPoint.Timestamp), 0),
	)

	return p
}

func writeBatchPoints(points client.BatchPoints) {
	pre := time.Now()
	for {
		err := influxClient.Write(points)
		duration := time.Since(pre)
		if err != nil {
			var iErr influxError
			err = json.Unmarshal([]byte(err.Error()), &iErr)
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "Unable to unmarshal error from InfluxDB. Error was: %v\n", err)
				time.Sleep(time.Duration(5) * time.Second) // give InfluxDB to recover
				continue
			}

			_, _ = fmt.Fprintf(os.Stderr, "Failed to write batch point. %v\n", iErr.Error)

			// If skip enabled or error is retention, skip
			if skipInfluxErrors || strings.Contains(iErr.Error, "points beyond retention") {
				//time.Sleep(time.Duration(5) * time.Second) // give InfluxDB to recover
				break
			} else {
				exit <- 2
				time.Sleep(time.Duration(100) * time.Second) // give other things chance to complete, and program to exit, without printing "committed"
			}
		}
		influxWriteTimer.Update(duration)
		break
	}
}
func influxWorker() {
	for abstractSerie := range influxSeries {
		// No point, ignore it
		if len(abstractSerie.Points) == 0 {
			continue
		}

		basename := strings.TrimSuffix(abstractSerie.Path[len(whisperDir)+1:], ".wsp")
		measure := strings.Replace(basename, "/", ".", -1)

		// Split measurement to find some useful things
		measureSplited := strings.Split(measure, ".")
		if len(measureSplited) <= 2 {
			log.Printf("Strange measure: %s. Ignoring.\n", measure)
			continue
		}

		// Reassemble without host
		measureKey := strings.Join(measureSplited[2:], "-")
		measureSplitedLen := len(measureSplited)
		measureName := measureSplited[1]

		bp, _ := client.NewBatchPoints(client.BatchPointsConfig{
			Precision: "s",
			Database:  influxDb,
		})

		for _, abstractPoint := range abstractSerie.Points {
			if p := transformWhisperPointToInfluxPoint(abstractPoint, measureName, measureKey, measureSplited,
				measureSplitedLen); p != nil {
				bp.AddPoint(p)

				// Write each 5000 points
				if len(bp.Points()) == 5000 {
					writeBatchPoints(bp)
					bp, _ = client.NewBatchPoints(client.BatchPointsConfig{
						Precision: "s",
						Database:  influxDb,
					})
				}
			}
		}

		if len(bp.Points()) != 0 {
			writeBatchPoints(bp)
		}
		finishedFiles <- abstractSerie.Path
	}
	influxWorkersWg.Done()
}

type abstractSerie struct {
	Path   string // used to keep track of ordering of processing
	Points []whisper.Point
}

func whisperWorker() {
	for path := range whisperFiles {
		fd, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Failed to open whisper file '%s': %s\n", path, err.Error())
			if skipWhisperErrors {
				continue
			} else {
				exit <- 2
			}
		}
		w, err := whisper.OpenWhisper(fd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Failed to open whisper file '%s': %s\n", path, err.Error())
			if skipWhisperErrors {
				continue
			} else {
				exit <- 2
			}
		}
		pre := time.Now()

		var duration time.Duration
		var points []whisper.Point
		if all {
			numTotalPoints := uint32(0)
			for i := range w.Header.Archives {
				numTotalPoints += w.Header.Archives[i].Points
			}
			points = make([]whisper.Point, 0, numTotalPoints)
			// iterate in backwards archive order (low res to high res)
			// so that if you write points of multiple archives to the same series, the high res ones will overwrite the low res ones
			for i := len(w.Header.Archives) - 1; i >= 0; i-- {
				allPoints, err := w.DumpArchive(i)
				if err != nil {
					fmt.Fprintf(os.Stderr, "ERROR: Failed to read archive %d in '%s', skipping: %s\n", i, path, err.Error())
					if skipWhisperErrors {
						continue
					} else {
						exit <- 2
					}
				}
				for _, point := range allPoints {
					// we have to filter out the "None" records (where we didn't fill in data) explicitly here!
					if point.Timestamp != 0 {
						points = append(points, point)
					}
				}
			}
			duration = time.Since(pre)
		} else {
			// not sure how it works, but i've emperically verified that this ignores null records, which is what we want
			// i.e. if whisper has a slot every minute, but you only have data every 3 minutes, we'll only process those records
			_, points, err = w.FetchUntil(fromTime, untilTime)
			duration = time.Since(pre)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to read file '%s' from %d to %d, skipping: %s (operation took %v)\n", path, fromTime, untilTime, err.Error(), duration)
				if skipWhisperErrors {
					w.Close()
					continue
				} else {
					exit <- 2
				}
			}
		}

		w.Close()

		whisperReadTimer.Update(duration)
		serie := &abstractSerie{path, points}
		influxSeries <- serie
	}
	whisperWorkersWg.Done()
}

func process(path string, info os.FileInfo, err error) error {
	// skipuntil can be "", in normal operation, or because we resumed operation.
	// if it's != "", it means user requested skipping and we haven't hit that entry yet
	if path == skipUntil {
		skipUntil = ""
		fmt.Printf("found '%s', disabling skipping.  skipped %d files\n", path, skipCounter)
	}
	if err != nil {
		return err
	}
	if !strings.HasSuffix(path, ".wsp") {
		return nil
	}
	if exclude != "" && strings.Contains(path, exclude) {
		return nil
	}
	if !strings.Contains(path, include) {
		return nil
	}

	if skipUntil != "" {
		skipCounter += 1
		return nil
	}

	foundFiles <- path
	whisperFiles <- path
	return nil
}

func init() {
	whisperFiles = make(chan string)
	influxSeries = make(chan *abstractSerie)
	foundFiles = make(chan string)
	finishedFiles = make(chan string)
	exit = make(chan int)

	whisperReadTimer = metrics.NewTimer()
	influxWriteTimer = metrics.NewTimer()
	metrics.Register("whisper_read", whisperReadTimer)
	metrics.Register("influx_write", influxWriteTimer)
}

func main() {
	now := uint(time.Now().Unix())
	yesterday := uint(time.Now().Add(-24 * time.Hour).Unix())

	flag.StringVar(&whisperDir, "whisperDir", "/opt/graphite/storage/whisper/", "location where all whisper files are stored")
	flag.IntVar(&influxWorkers, "influxWorkers", 10, "specify how many influx workers")
	flag.IntVar(&whisperWorkers, "whisperWorkers", 10, "specify how many whisper workers")
	flag.UintVar(&from, "from", yesterday, "Unix epoch time of the beginning of the requested interval. (default: 24 hours ago). ignored if all=true")
	flag.UintVar(&until, "until", now, "Unix epoch time of the end of the requested interval. (default: now). ignored if all=true")
	flag.StringVar(&influxHost, "influxHost", "localhost", "influxdb host")
	flag.UintVar(&influxPort, "influxPort", 8086, "influxdb port")
	flag.StringVar(&influxUser, "influxUser", "graphite", "influxdb user")
	flag.StringVar(&influxPass, "influxPass", "graphite", "influxdb pass")
	flag.StringVar(&influxDb, "influxDb", "graphite", "influxdb database")
	flag.StringVar(&skipUntil, "skipUntil", "", "absolute path of a whisper file from which to resume processing")
	flag.StringVar(&influxPrefix, "influxPrefix", "", "prefix this string to all imported data")
	flag.StringVar(&include, "include", "", "only process whisper files whose filename contains this string (\"\" is a no-op, and matches everything")
	flag.StringVar(&exclude, "exclude", "", "don't process whisper files whose filename contains this string (\"\" disables the filter, and matches nothing")
	flag.BoolVar(&verbose, "verbose", false, "verbose output")
	flag.BoolVar(&all, "all", false, "copy all data from all archives, as opposed to just querying the timerange from the best archive")
	flag.BoolVar(&skipInfluxErrors, "skipInfluxErrors", false, "when an influxdb write fails, skip to the next one istead of failing")
	flag.BoolVar(&skipWhisperErrors, "skipWhisperErrors", false, "when a whisper read fails, skip to the next one instead of failing")
	flag.UintVar(&statsInterval, "statsInterval", 10, "interval to display stats. by default 10 seconds.")

	flag.Parse()

	if strings.HasSuffix(whisperDir, "/") {
		whisperDir = whisperDir[:len(whisperDir)-1]
	}
	fromTime = uint32(from)
	untilTime = uint32(until)

	cfg := client.HTTPConfig{
		Addr:     fmt.Sprintf("http://%s:%d", influxHost, influxPort),
		Username: influxUser,
		Password: influxPass,
	}

	var err error
	influxClient, err = client.NewHTTPClient(cfg)
	if err != nil {
		log.Fatal(err)
	}

	// i wish there was a way to enforce that logs gets displayed right before we quit
	go metrics.Log(metrics.DefaultRegistry, time.Duration(statsInterval)*time.Second, log.New(os.Stderr, "metrics: ", log.Lmicroseconds))

	for i := 1; i <= influxWorkers; i++ {
		influxWorkersWg.Add(1)
		go influxWorker()
	}
	for i := 1; i <= whisperWorkers; i++ {
		whisperWorkersWg.Add(1)
		go whisperWorker()
	}

	go keepOrder()

	err = filepath.Walk(whisperDir, process)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		exit <- 2
	}
	if verbose {
		fmt.Println("fileWalk is done. closing channel")
	}
	close(whisperFiles)
	if verbose {
		fmt.Println("waiting for whisperworkers to finish")
	}
	whisperWorkersWg.Wait()
	close(influxSeries)
	if verbose {
		fmt.Println("waiting for influxworkers to finish")
	}
	influxWorkersWg.Wait()
	if verbose {
		fmt.Println("all done. exiting")
	}
}
