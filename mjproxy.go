package main

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"github.com/gorilla/mux"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/http/pprof"
	"net/url"
	"sync"
	"time"
)

const BindAddress = ":4000"
const DefaultTimeout = 300
const TimeOutThrotling = 60 * 1

type ProxyRequestItem struct {
	Id      string `json:"id"`
	Url     string `json:"url"`
	Method  string `json:"method"`
	Params  string `json:"params"`
	Timeout int    `json:"timeout"`
}

func (r *ProxyRequestItem) getOrigin() (string, error) {
	origin := ``
	Url, err := url.Parse(r.Url)

	if err == nil {
		origin = Url.Scheme + `//` + Url.Host
	}
	return origin, err
}

func (r *ProxyRequestItem) getRequestHash() []byte {
	h := md5.New()
	io.WriteString(h, r.Id)
	io.WriteString(h, "-")
	io.WriteString(h, r.Url)
	io.WriteString(h, "-")
	io.WriteString(h, string(rand.Int63()))
	io.WriteString(h, "-")
	io.WriteString(h, time.Now().String())
	return h.Sum(nil)
}

func (r *ProxyRequestItem) getHostHash() ([]byte, error) {
	h := md5.New()
	origin, err := r.getOrigin()

	io.WriteString(h, origin)

	return h.Sum(nil), err
}

type ProxyRequest []ProxyRequestItem

type RequestResult struct {
	TaskHash   string           `json:"task_hash"`
	TaskStatus string           `json:"task_status"`
	Request    ProxyRequestItem `json:"request"`
	Result     struct {
		HttpStatus int    `json:"http_status"`
		ResultBody string `json:"body"`
	} `json:"result"`
	Created  time.Time `json:"created"`
	Finished time.Time `json:"finished"`
}

type ProxyResponseItem struct {
	Success     bool   `json:"success"`
	Id          string `json:"id"`
	Url         string `json:"url"`
	HttpStatus  int    `json:"http_status"`
	Duration    string `json:"duration"`
	ResultBody  string `json:"body"`
	Message     string `json:"message"`
	requestHash string
}

type ProxyResponse []ProxyResponseItem

type HostState struct {
	m                   sync.RWMutex
	Origin              string        `json: origin`
	TimeoutCounter      int           `json: timoutCounter`
	LastTimeout         time.Time     `json: lastTimeout`
	RequestCount        int64         `json: requestCount`
	RequestSuccessCount int64         `json: requestSuccessCount`
	LastDuration        time.Duration `json: lastDuration`
	SumSuccessDuration  time.Duration `json: sumSuccessDuration`
	SumDuration         time.Duration `json: sumDuration`
}

var HostsContainer struct {
	m     sync.RWMutex
	Hosts map[string]HostState `json: hosts`
}

var tasksChan = make(chan []byte, 50000) // why 50000?

func main() {
	HostsContainer.Hosts = make(map[string]HostState)

	r := mux.NewRouter()
	r.HandleFunc("/", proxy).Methods("POST").HeadersRegexp("Content-Type", "application/(json|javascript)")
	r.HandleFunc("/stat", stat)
	r.PathPrefix("/debug/pprof/").HandlerFunc(pprof.Index)

	server := &http.Server{
		Addr:        BindAddress,
		Handler:     r,
		ReadTimeout: 3000 * time.Millisecond,
		//WriteTimeout: 3000 * time.Millisecond,
	}

	log.Println("Proxy starting----------------------------------------------------------------------------")
	log.Println("Listening on ", BindAddress)
	log.Println(server.ListenAndServe())
}

/**
- get json request from client
[
	{
		"id": "" // simple url identifire,
		"url": "http://....",
		"timeout": 1000, // in ms
	},
	...
]
*/
func proxy(w http.ResponseWriter, r *http.Request) {
	//log.Println("Proxy started===========================================================================")
	w.Header().Set("Content-Type", "application/json")
	//log.Println("Handle request", r.URL, len(tasksChan), cap(tasksChan))

	//if len(tasksChan) == cap(tasksChan) {
	//	w.Write([]byte("Service is overloaded"))
	//	//log.Println("Service is overloaded")
	//	return
	//}

	requestBody, err := ioutil.ReadAll(r.Body)
	if err != nil {
		//log.Println("Body read eror", err)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Body read error"))
		return
	}

	var proxyRequest ProxyRequest

	err = json.Unmarshal(requestBody, &proxyRequest)
	if err != nil {
		//log.Println("Unable to unmarshal json", err)
		w.Write([]byte("Unable to unmarshal json"))
		return
	}

	//log.Println(proxyRequest, len(proxyRequest), cap(proxyRequest))

	var requestChan = make(chan ProxyRequestItem, len(proxyRequest))
	var responsesChan = make(chan ProxyResponseItem, len(proxyRequest))
	var requestItem ProxyRequestItem
	var proxyResponse = ProxyResponse{}

	if len(proxyRequest) > 0 {
		//log.Println(`Start task producer goroutine`)
		var SkipCount struct {
			m     sync.RWMutex
			Count int
		}

		go func() {
			//log.Println("First goroutine started ===========================================================================")
			for _, requestItem = range proxyRequest {
				//log.Println(`Send request to requestChan`, requestItem)
				requestChan <- requestItem
			}
			i := 0
			for {
				time.Sleep(1 * time.Millisecond)
				i++
				SkipCount.m.RLock()
				if len(proxyResponse)+SkipCount.Count == len(proxyRequest) || i == 60*1000 {
					SkipCount.m.RUnlock()
					break
				}
				SkipCount.m.RUnlock()
			}
			//log.Println(`All request done for (ms)`, i)
			close(requestChan)
			close(responsesChan)
		}()

		//log.Println(`Start task processor goroutines`)
		go func() {
			//log.Println(`Task processor goroutine ===========================================================================`)
			for {
				request, ok := <-requestChan
				if !ok {
					//log.Println(`requestChan closed`)
					break
				}
				hostHash, err := request.getHostHash()
				origin, err := request.getOrigin()
				if err != nil {
					//log.Println(err.Error())
					continue
				}
				hostHashStr := base64.StdEncoding.EncodeToString([]byte(hostHash))
				reqHashStr := base64.StdEncoding.EncodeToString(request.getRequestHash())

				//log.Println(reqHashStr, ` RLock `)
				HostsContainer.m.RLock()

				hostInfo, ok := HostsContainer.Hosts[string(hostHashStr)]
				if ok && hostInfo.LastTimeout.Add(TimeOutThrotling*time.Second).After(time.Now()) {
					SkipCount.m.Lock()
					SkipCount.Count++
					//log.Println(reqHashStr, ` Skip `, request.Url, SkipCount.Count)
					SkipCount.m.Unlock()
					//log.Println(reqHashStr, ` RUnlock `)
					HostsContainer.m.RUnlock()
					continue
				}

				//log.Println(reqHashStr, ` RUnlock `)
				HostsContainer.m.RUnlock()

				go func() {
					//log.Println(reqHashStr, ` Start task processor goroutine`, request.Url)
					responseItem := ProxyResponseItem{
						Url:         request.Url,
						Id:          request.Id,
						requestHash: reqHashStr,
					}

					//log.Println(reqHashStr, ` get http response`)
					timeout := time.Duration(request.Timeout) * time.Millisecond
					//log.Println(reqHashStr, ` timeout: `, timeout, request.Timeout)
					if request.Timeout == 0 {
						timeout = time.Duration(DefaultTimeout) * time.Millisecond
					}
					start := time.Now()

					client := http.Client{
						Timeout: timeout,
					}

					response, err := client.Get(request.Url) //, values
					finish := time.Now()

					//log.Println(reqHashStr, ` Lock `)
					HostsContainer.m.Lock()
					hostInfo, ok := HostsContainer.Hosts[hostHashStr]
					if !ok {
						hostInfo = HostState{
							Origin:       origin,
							LastTimeout:  time.Unix(0, 0),
							RequestCount: 0,
						}
						HostsContainer.Hosts[hostHashStr] = hostInfo
					}
					hostInfo.RequestCount++

					if err != nil {
						hostInfo.LastTimeout = time.Now()
						hostInfo.TimeoutCounter++
						responseItem.Success = false
						responseItem.Message = err.Error()
						responseItem.ResultBody = ``
						responseItem.Duration = finish.Sub(start).String()
						//log.Println(reqHashStr, ` http.get error`, err.Error())
					} else {
						responseItem.Success = true
						responseItem.HttpStatus = response.StatusCode
						responseItem.Duration = finish.Sub(start).String()
						hostInfo.RequestSuccessCount++
						hostInfo.SumSuccessDuration = time.Duration(hostInfo.SumDuration + finish.Sub(start))

						respBody, err := ioutil.ReadAll(response.Body)
						responseItem.ResultBody = string(respBody)
						if err != nil {
							responseItem.Success = false
							responseItem.Message = ` read result body failed`
							responseItem.ResultBody = err.Error()
						}
					}
					hostInfo.SumDuration = time.Duration(hostInfo.SumDuration + finish.Sub(start))
					HostsContainer.Hosts[hostHashStr] = hostInfo

					//log.Println(reqHashStr, ` execution time `, finish.Sub(start).String(), ` sending http response to responsesChan`)

					//log.Println(reqHashStr, ` Unlock`)
					HostsContainer.m.Unlock()

					responsesChan <- responseItem
				}()
			}
		}()

		for {
			//log.Println(`Waiting for responsesChan`, len(responsesChan), cap(responsesChan))
			responseItem, ok := <-responsesChan
			if !ok {
				//log.Println(`responsesChan closed`)
				break
			}

			//log.Println(responseItem.requestHash, ` responseItem finished`)
			proxyResponse = append(proxyResponse, responseItem)
		}

		//log.Println(`Sending response to client`, len(proxyResponse))

		enc := json.NewEncoder(w)
		err := enc.Encode(proxyResponse)
		if err != nil {
			//log.Println(`json encode error`, err.Error())
		}
		return
	}

	enc := json.NewEncoder(w)
	enc.Encode(`[]`)
	//log.Println(`Sending empty response to client`)
	return
}

func stat(w http.ResponseWriter, r *http.Request) {
	//log.Println(`RLock()`)
	HostsContainer.m.RLock()
	enc := json.NewEncoder(w)
	err := enc.Encode(HostsContainer)
	if err != nil {
		//log.Println(`json encode error`, err.Error())
	}
	//log.Println(`RUnlock()`)
	HostsContainer.m.RUnlock()
	return
}
