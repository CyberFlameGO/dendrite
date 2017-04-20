package main

import (
	"flag"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

const usage = `Usage: %s

Create a single endpoint URL which clients can be pointed at.

The client-server API in Dendrite is split across multiple processes
which listen on multiple ports. You cannot point a Matrix client at
any of those ports, as there will be unimplemented functionality.
In addition, all client-server API processes start with the additional
path prefix '/api', which Matrix clients will be unaware of.

This tool will proxy requests for all client-server URLs and forward
them to their respective process. It will also add the '/api' path
prefix to incoming requests.

THIS TOOL IS FOR TESTING AND NOT INTENDED FOR PRODUCTION USE.

Arguments:

`

var (
	syncServerURL = flag.String("sync-api-server-url", "", "The base URL of the listening 'dendrite-sync-api-server' process. E.g. 'http://localhost:4200'")
	clientAPIURL  = flag.String("client-api-server-url", "", "The base URL of the listening 'dendrite-client-api-server' process. E.g. 'http://localhost:4321'")
	bindAddress   = flag.String("bind-address", ":8008", "The listening port for the proxy.")
)

func makeProxy(targetURL string) (*httputil.ReverseProxy, error) {
	if !strings.HasSuffix(targetURL, "/") {
		targetURL += "/"
	}
	// Check that we can parse the URL.
	_, err := url.Parse(targetURL)
	if err != nil {
		return nil, err
	}
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// URL.Path() removes the % escaping from the path.
			// The % encoding will be added back when the url is encoded
			// when the request is forwarded.
			// This means that we will lose any unessecary escaping from the URL.
			// Pratically this means that any distinction between '%2F' and '/'
			// in the URL will be lost by the time it reaches the target.
			path := req.URL.Path
			path = "api" + path
			log.WithFields(log.Fields{
				"path":   path,
				"url":    targetURL,
				"method": req.Method,
			}).Print("proxying request")
			newURL, err := url.Parse(targetURL + path)
			if err != nil {
				// We already checked that we can parse the URL
				// So this shouldn't ever get hit.
				panic(err)
			}
			// Copy the query parameters from the request.
			newURL.RawQuery = req.URL.RawQuery
			req.URL = newURL
		},
	}, nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, usage, os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()

	if *syncServerURL == "" {
		flag.Usage()
		fmt.Fprintln(os.Stderr, "no --sync-server-url specified.")
		os.Exit(1)
	}

	if *clientAPIURL == "" {
		flag.Usage()
		fmt.Fprintln(os.Stderr, "no --client-api-url specified.")
		os.Exit(1)
	}

	syncProxy, err := makeProxy(*syncServerURL)
	if err != nil {
		panic(err)
	}
	clientProxy, err := makeProxy(*clientAPIURL)
	if err != nil {
		panic(err)
	}

	http.Handle("/_matrix/client/r0/sync", syncProxy)
	http.Handle("/", clientProxy)

	srv := &http.Server{
		Addr:         *bindAddress,
		ReadTimeout:  1 * time.Minute, // how long we wait for the client to send the entire request (after connection accept)
		WriteTimeout: 5 * time.Minute, // how long the proxy has to write the full response
	}

	fmt.Println("Proxying requests to:")
	fmt.Println("  /_matrix/client/r0/sync  => ", *syncServerURL+"/api/_matrix/client/r0/sync")
	fmt.Println("  /*                       => ", *clientAPIURL+"/api/*")
	fmt.Println("Listening on ", *bindAddress)
	srv.ListenAndServe()

}
