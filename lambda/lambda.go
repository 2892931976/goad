package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/gophergala2016/goad/sqsadaptor"
)

func main() {
	address := os.Args[1]
	concurrencycount, err := strconv.Atoi(os.Args[2])
	if err != nil {
		fmt.Printf("ERROR %s\n", err)
		return
	}
	maxRequestCount, err := strconv.Atoi(os.Args[3])
	sqsurl := os.Args[4]
	awsregion := os.Args[5]
	clientTimeout, _ := time.ParseDuration("1s")
	if len(os.Args) >= 7 {
		clientTimeout, _ = time.ParseDuration(os.Args[6])
	}
	if err != nil {
		fmt.Printf("ERROR %s\n", err)
		return
	}
	client := &http.Client{}
	client.Timeout = clientTimeout
	fmt.Printf("Will spawn %d workers making %d requests to %s\n", concurrencycount, maxRequestCount, address)
	runLoadTest(client, sqsurl, address, maxRequestCount, concurrencycount, awsregion)
}

type Job struct{}

type RequestResult struct {
	Time             int64  `json:"time"`
	Host             string `json:"host"`
	Type             string `json:"type"`
	Status           int    `json:"status"`
	ElapsedFirstByte int64  `json:"elapsed-first-byte"`
	ElapsedLastByte  int64  `json:"elapsed-last-byte"`
	Elapsed          int64  `json:"elapsed"`
	Bytes            int    `json:"bytes"`
	Timeout          bool   `json:"timeout"`
	ConnectionError  bool   `json:"connection-error"`
	State            string `json:"state"`
}

func runLoadTest(client *http.Client, sqsurl string, url string, totalRequests int, concurrencycount int, awsregion string) {
	awsConfig := aws.NewConfig().WithRegion(awsregion)
	sqsAdaptor := sqsadaptor.NewSQSAdaptor(awsConfig, sqsurl)
	//sqsAdaptor := sqsadaptor.NewDummyAdaptor(sqsurl)
	jobs := make(chan Job, totalRequests)
	ch := make(chan RequestResult, totalRequests)
	var wg sync.WaitGroup
	loadTestStartTime := time.Now()
	var requestsSoFar int
	for i := 0; i < totalRequests; i++ {
		jobs <- Job{}
	}
	close(jobs)
	for i := 0; i < concurrencycount; i++ {
		wg.Add(1)
		go fetch(loadTestStartTime, client, url, totalRequests, jobs, ch, &wg, awsregion)
	}
	fmt.Println("Waiting for results…")

	for requestsSoFar < totalRequests {
		var agg [100]RequestResult
		aggRequestCount := len(agg)
		i := 0

		var timeToFirstTotal int64
		var requestTimeTotal int64
		totBytesRead := 0
		statuses := make(map[string]int)
		var firstRequestTime int64
		var lastRequestTime int64
		var slowest int64
		var fastest int64
		var totalTimedOut int
		var totalConnectionError int
		for ; i < aggRequestCount && requestsSoFar < totalRequests; i++ {
			r := <-ch
			agg[i] = r
			requestsSoFar++
			if requestsSoFar%10 == 0 {
				fmt.Printf("\r%.2f%% done (%d requests out of %d)", (float64(requestsSoFar)/float64(totalRequests))*100.0, requestsSoFar, totalRequests)
			}
			if firstRequestTime == 0 {
				firstRequestTime = r.Time
			}

			lastRequestTime = r.Time

			if r.Timeout {
				totalTimedOut++
				continue
			}
			if r.ConnectionError {
				totalConnectionError++
				continue
			}

			if r.ElapsedLastByte > slowest {
				slowest = r.ElapsedLastByte
			}
			if fastest == 0 {
				fastest = r.ElapsedLastByte
			} else {
				if r.ElapsedLastByte < fastest {
					fastest = r.ElapsedLastByte
				}
			}

			timeToFirstTotal += r.ElapsedFirstByte
			totBytesRead += r.Bytes
			statusStr := strconv.Itoa(r.Status)
			_, ok := statuses[statusStr]
			if !ok {
				statuses[statusStr] = 1
			} else {
				statuses[statusStr]++
			}
			requestTimeTotal += r.Elapsed
		}
		if i == 0 {
			continue
		}
		durationNanoSeconds := lastRequestTime - firstRequestTime
		durationSeconds := float32(durationNanoSeconds) / float32(1000000000)
		aggData := sqsadaptor.AggData{
			i,
			totalTimedOut,
			totalConnectionError,
			timeToFirstTotal / int64(i),
			totBytesRead,
			statuses,
			requestTimeTotal / int64(i),
			float32(i) / durationSeconds,
			slowest,
			fastest,
			awsregion,
		}
		sqsAdaptor.SendResult(aggData)
	}
	fmt.Printf("\nWaiting for workers…")
	wg.Wait()
	fmt.Printf("\nYay🎈  - %d requests completed\n", requestsSoFar)

}

func fetch(loadTestStartTime time.Time, client *http.Client, address string, requestcount int, jobs <-chan Job, ch chan RequestResult, wg *sync.WaitGroup, awsregion string) {
	defer wg.Done()
	fmt.Printf("Fetching %s\n", address)
	for _ = range jobs {
		start := time.Now()
		req, err := http.NewRequest("GET", address, nil)
		req.Header.Add("User-Agent", "GOAD/0.1")
		response, err := client.Do(req)
		var status string
		var elapsedFirstByte time.Duration
		var elapsedLastByte time.Duration
		var elapsed time.Duration
		var statusCode int
		var bytesRead int
		buf := []byte(" ")
		timedOut := false
		connectionError := false
		if err != nil {
			status = fmt.Sprintf("ERROR: %s\n", err)
			//fmt.Println(status)
			switch err := err.(type) {
			case *url.Error:
				if err, ok := err.Err.(net.Error); ok && err.Timeout() {
					timedOut = true
				}
			case net.Error:
				if err.Timeout() {
					timedOut = true
				}
			}

			if !timedOut {
				connectionError = true
			}
		} else {
			statusCode = response.StatusCode
			_, err = response.Body.Read(buf)
			if err != nil {
				status = fmt.Sprintf("reading first byte failed: %s\n", err)
			}
			elapsedFirstByte = time.Since(start)
			body, err := ioutil.ReadAll(response.Body)
			bytesRead = len(body) + 1
			elapsedLastByte = time.Since(start)
			if err != nil {
				status = fmt.Sprintf("reading response body failed: %s\n", err)
			} else {
				status = "Success"
			}
			elapsed = time.Since(start)
		}
		result := RequestResult{
			start.Sub(loadTestStartTime).Nanoseconds(),
			req.URL.Host,
			req.Method,
			statusCode,
			elapsed.Nanoseconds(),
			elapsedFirstByte.Nanoseconds(),
			elapsedLastByte.Nanoseconds(),
			bytesRead,
			timedOut,
			connectionError,
			status,
		}
		ch <- result
	}
}
