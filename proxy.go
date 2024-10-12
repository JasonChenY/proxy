package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var backendsMutex sync.Mutex
var backends = []string{
	"http://www.baidu.com",
	"http://www.qq.com",
	"http://www.taobao.com",
	"http://www.google.com",
	"http://www.jianshu.com",
	"http://www.notexist4.com",
	"http://www.notexist2.com",
	"http://www.notexist3.com",
}
var backendFallbacks = []string{}

var batchSize = 2

func random(backends []string) []string {
	ret := []string{}
	r := rand.New(rand.NewSource(time.Now().Unix()))

	for _, i := range r.Perm(len(backends)) {
		val := backends[i]
		ret = append(ret, val)
	}
	return ret
}

// Hop-by-hop headers. These are removed when sent to the backend.
// http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection", // non-standard but still sent by libcurl and rejected by e.g. google
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",      // canonicalized version of "TE"
	"Trailer", // not Trailers per URL above; http://www.rfc-editor.org/errata_search.php?eid=4522
	"Transfer-Encoding",
	"Upgrade",
}

func removeHeaders(header http.Header) {
	// Remove hop-by-hop headers listed in the "Connection" header.
	if c := header.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				header.Del(f)
			}
		}
	}

	// Remove hop-by-hop headers
	for _, h := range hopHeaders {
		if header.Get(h) != "" {
			header.Del(h)
		}
	}
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

func director(target *url.URL, req *http.Request, stripPrefix bool) {
	targetQuery := target.RawQuery

	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host

	path := req.URL.Path
	if stripPrefix {
		array := strings.Split(path, "/")
		if len(array) > 2 {
			path = filepath.Join(array[2:]...)
		}
	}
	req.URL.Path = singleJoiningSlash(target.Path, path)

	// If Host is empty, the Request.Write method uses
	// the value of URL.Host.
	// force use URL.Host
	req.Host = req.URL.Host
	if targetQuery == "" || req.URL.RawQuery == "" {
		req.URL.RawQuery = targetQuery + req.URL.RawQuery
	} else {
		req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
	}

	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header.Set("User-Agent", "")
	}

	req.RequestURI = ""
}

func addXForwardedForHeader(req *http.Request) {
	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		if prior, ok := req.Header["X-Forwarded-For"]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		req.Header.Set("X-Forwarded-For", clientIP)
	}
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
func copyResponse(dst io.Writer, src io.Reader) {
	io.Copy(dst, src)
}

func innerHandler(rw http.ResponseWriter, req *http.Request, servers []string, stripPrefix bool) bool {
	var found bool = false
	for i := 0; i < len(servers); i += batchSize {
		var lock sync.Mutex
		var wg sync.WaitGroup
		var timeout bool = false
		var batch = servers[i:min(i+batchSize, len(servers))]
		var cancels = make([]context.CancelFunc, batchSize)

		for j, backend := range batch {
			wg.Add(1)
			go func(myindex int, backend string) {
				defer wg.Done()

				cx, cancel := context.WithCancel(context.Background())
				cancels[myindex] = cancel

				backendURL, err := url.Parse(backend)
				if err != nil {
					fmt.Println("invalid backend url ", backend)
					return
				}

				/*
					not deep copy of all fields, will cause incorrect req sent in concurrent situation.
					outreq := new(http.Request)
					*outreq = *req
				*/

				outreq := req.Clone(cx)
				director(backendURL, outreq, stripPrefix)
				fmt.Println(outreq.URL.String())
				outreq.Close = false

				outreq.Header = make(http.Header)
				copyHeader(outreq.Header, req.Header)

				// Remove hop-by-hop headers listed in the "Connection" header, Remove hop-by-hop headers.
				removeHeaders(outreq.Header)

				// Add X-Forwarded-For Header.
				addXForwardedForHeader(outreq)
				//outreq = outreq.WithContext(cx)

				res, err := http.DefaultTransport.RoundTrip(outreq)

				if err == nil && (res.StatusCode == http.StatusOK || res.StatusCode == http.StatusPartialContent) {
					lock.Lock()

					if !found {
						fmt.Println(backend, " get ok response")
						found = true

						// unlock asap to let other goroutine abort
						lock.Unlock()

						// cancel other ongoing request asap
						for k, c := range cancels {
							if k != myindex {
								if c != nil {
									c()
								}
							}
						}
					} else if found {
						// notify the channel asap to cancel the other ongoing requests.
						// but in very rare cases, cant ensure receive two or more successful response, then WriteHeaders will get "http: superfluous response.WriteHeader call" error and even SIGSEGV.
						// use non-buffered channel as mutex for sync, also cant not solve problem completely!
						fmt.Println(backend, " get ok response after someother, return")
						res.Body.Close()
						lock.Unlock()
						return
					} else if timeout {
						fmt.Println(backend, " get ok response after timeout, abort")
						res.Body.Close()
						lock.Unlock()
						return
					}

					// Remove hop-by-hop headers listed in the "Connection" header of the response, Remove hop-by-hop headers.
					removeHeaders(res.Header)

					// Copy header from response to client.
					copyHeader(rw.Header(), res.Header)

					// The "Trailer" header isn't included in the Transport's response, Build it up from Trailer.
					if len(res.Trailer) > 0 {
						trailerKeys := make([]string, 0, len(res.Trailer))
						for k := range res.Trailer {
							trailerKeys = append(trailerKeys, k)
						}
						rw.Header().Add("Trailer", strings.Join(trailerKeys, ", "))
					}

					rw.WriteHeader(res.StatusCode)
					if len(res.Trailer) > 0 {
						// Force chunking if we saw a response trailer.
						// This prevents net/http from calculating the length for short
						// bodies and adding a Content-Length.
						if fl, ok := rw.(http.Flusher); ok {
							fl.Flush()
						}
					}

					copyResponse(rw, res.Body)
					// close now, instead of defer, to populate res.Trailer
					res.Body.Close()
					copyHeader(rw.Header(), res.Trailer)
				} else {
					if err != nil {
						fmt.Println(backend, err)
					} else {
						res.Body.Close()
						fmt.Println(backend, res.StatusCode)
					}
				}
			}(j, backend)
		}

		allFinished := make(chan struct{})
		go func() {
			wg.Wait()
			allFinished <- struct{}{}
		}()

		select {
		case <-time.After(20 * time.Second):
			lock.Lock()
			timeout = true
			lock.Unlock()
			fmt.Println("no backend succeed before timeout, abort ongoing tasks")
			for _, c := range cancels {
				if c != nil {
					c()
				}
			}
		case <-allFinished:
			//fmt.Println("all backend goroutine quit")
		}

		if found {
			break
		} else {
			fmt.Println("try next batch")
		}
	}
	return found
}

func myhandler(rw http.ResponseWriter, req *http.Request) {
	fmt.Println(req.URL.String())

	backendsMutex.Lock()
	randomBackends := random(backends)
	backendsMutex.Unlock()

	var found bool = innerHandler(rw, req, randomBackends, false)
	if !found {
		found = innerHandler(rw, req, backendFallbacks, true)
	}
	if !found {
		rw.WriteHeader(http.StatusNotFound)
	}

	fmt.Println("\n")
	//fmt.Println("当前运行的goroutine数量:", runtime.NumGoroutine())
}

func fillBackendsFromSts() bool {
	backendNamespace := os.Getenv("BACKEND_NAMESPACE")
	backendHeadlessService := os.Getenv("BACKEND_HEADLESS_SERVICE")
	backendStatefulset := os.Getenv("BACKEND_NAME")
	backendScheme := os.Getenv("BACKEND_SCHEME")
	if len(backendScheme) == 0 {
		backendScheme = "http"
	}

	backendPort := os.Getenv("BACKEND_PORT")
	if len(backendPort) == 0 {
		backendPort = "80"
	}

	replicasStr := os.Getenv(("BACKEND_STATEFULSET_REPLICAS"))
	replicas, err := strconv.Atoi(replicasStr)
	if err != nil {
		fmt.Println("转换失败:", err)
		return false
	}

	if len(backendNamespace) > 0 && len(backendHeadlessService) > 0 && len(backendStatefulset) > 0 && replicas > 0 {
		backends = backends[:0]
		for i := 0; i < replicas; i++ {
			backends = append(backends, fmt.Sprintf("%s://%s-%d.%s.%s.svc.cluster.local:%s", backendScheme, backendStatefulset, i, backendHeadlessService, backendNamespace, backendPort))
		}
	} else {
		fmt.Println("BACKEND_NAMESPACE/BACKEND_HEADLESS_SERVICE/BACKEND_NAME/BACKEND_STATEFULSET_REPLICAS environment variables necessary")
		return false
	}
	return true
}

func main() {
	backendFallbacksStr := os.Getenv("BACKEND_FALLBACKS")
	if len(backendFallbacksStr) > 0 {
		backendFallbacks = strings.Split(backendFallbacksStr, ";")
	}

	batchsizeStr := os.Getenv("BATCHSIZE")
	size, err := strconv.Atoi(batchsizeStr)
	if err != nil {
		fmt.Println("转换失败:", err)
		return
	}
	batchSize = size

	backendtype := os.Getenv("BACKEND_WORKLOAD_TYPE")
	if backendtype == "statefulset" {
		fillBackendsFromSts()
	} else if backendtype == "daemonset" {
		backendNamespace := os.Getenv("BACKEND_NAMESPACE")
		backendDaemonset := os.Getenv("BACKEND_NAME")
		if len(backendNamespace) > 0 && len(backendDaemonset) > 0 {
			watchPodForDs(backendNamespace, backendDaemonset, &backends, backendsMutex)
		} else {
			fmt.Println("BACKEND_NAMESPACE/BACKEND_DAEMONSET necessary")
		}
	} else {
		fmt.Println("running in test mode")
	}

	fmt.Println(backends)

	http.HandleFunc("/", myhandler)
	fmt.Println(http.ListenAndServe(":8080", nil))
}
