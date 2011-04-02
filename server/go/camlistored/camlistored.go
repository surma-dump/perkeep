/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"http"
	"log"
	"strings"
	"os"

	"camli/auth"
	"camli/blobref"
	"camli/client"
	"camli/httputil"
	"camli/webserver"
	"camli/blobserver"
	"camli/blobserver/handlers"
	"camli/search"

	// Storage options:
	"camli/blobserver/localdisk"
	_ "camli/blobserver/s3"
	"camli/mysqlindexer"  // indexer, but uses storage interface
)

var flagUseConfigFiles = flag.Bool("useconfigfiles", false,
	"Use the ~/.camli/config files and enable the /config HTTP handler." +
	"+If false, all configuration is done ")
var flagPasswordFile = flag.String("passwordfile", "password.txt",
	"Password file, relative to the ~USER/.camli/ directory.")

// If useConfigFiles is off:
var flagStorageRoot = flag.String("root", "/tmp/camliroot", "Root directory to store files")
var flagQueuePartitions = flag.String("queue-partitions", "queue-indexer",
	"Comma-separated list of queue partitions to reference uploaded blobs into. "+
		"Typically one for your indexer and one per mirror full syncer.")
// TODO: Temporary
var flagRequestLog = flag.Bool("reqlog", false, "Log incoming requests")
var flagDevMySql = flag.Bool("devmysqlindexer", false, "Temporary option to enable MySQL indexer on /indexer")
var flagDevSearch = flag.Bool("devsearch", false, "Temporary option to enable search interface at /camli/search")
var flagDatabaseName = flag.String("dbname", "devcamlistore", "MySQL database name")

var storage blobserver.Storage

const camliPrefix = "/camli/"
const partitionPrefix = "/partition-"

var InvalidCamliPath = os.NewError("Invalid Camlistore request path")

var _ blobserver.Partition = &partitionConfig{}
var mainPartition = &partitionConfig{"", true, true, false, nil, "http://localhost"}

func parseCamliPath(path string) (partitionName string, action string, err os.Error) {
	camIdx := strings.Index(path, camliPrefix)
	if camIdx == -1 {
		err = InvalidCamliPath
		return
	}
	action = path[camIdx+len(camliPrefix):]
	if camIdx == 0 {
		return
	}
	if !strings.HasPrefix(path, partitionPrefix) {
		err = InvalidCamliPath
		return
	}
	partitionName = path[len(partitionPrefix):camIdx]
	if !isValidPartitionName(partitionName) {
		err = InvalidCamliPath
	return
	}
	return
}

func pickPartitionHandlerMaybe(req *http.Request) (handler http.HandlerFunc, intercept bool) {
	if !strings.HasPrefix(req.URL.Path, partitionPrefix) {
		intercept = false
		return
	}
	return http.HandlerFunc(handleCamli), true
}

func unsupportedHandler(conn http.ResponseWriter, req *http.Request) {
	httputil.BadRequestError(conn, "Unsupported camlistore path or method.")
}

func handleCamli(conn http.ResponseWriter, req *http.Request) {
	partName, action, err := parseCamliPath(req.URL.Path)
	if err != nil {
		log.Printf("Invalid request for method %q, path %q",
			req.Method, req.URL.Path)
		unsupportedHandler(conn, req)
		return
	}
	partition := queuePartitionMap[partName]
	if partition == nil {
		httputil.BadRequestError(conn, "Unconfigured partition.")
		return
	}
	handleCamliUsingStorage(conn, req, action, partition, storage)
}

func makeIndexHandler(storage blobserver.Storage) func(conn http.ResponseWriter, req *http.Request) {
	const prefix = "/indexer"
	partition := &partitionConfig{
		name:     "indexer",
		writable: true,
		readable: false,
		queue:    false,
		urlbase:  mainPartition.urlbase + prefix,
	}
	return func(conn http.ResponseWriter, req *http.Request) {
		if !strings.HasPrefix(req.URL.Path, prefix) {
			panic("bogus request")
			return
		}

		path := req.URL.Path[len(prefix):]
		_, action, err := parseCamliPath(path)
		if err != nil {
			log.Printf("Invalid request for method %q, path %q",
				req.Method, req.URL.Path)
			unsupportedHandler(conn, req)
			return
		}
		log.Printf("INDEXER action %s on partition %q", action, partition)
		handleCamliUsingStorage(conn, req, action, partition, storage)
	}
}

func handleCamliUsingStorage(conn http.ResponseWriter, req *http.Request, action string, partition blobserver.Partition, storage blobserver.Storage) {
	handler := unsupportedHandler
	if *flagRequestLog {
		log.Printf("method %q; partition %q; action %q", req.Method, partition, action)
	}
	switch req.Method {
	case "GET":
		switch action {
		case "enumerate-blobs":
			handler = auth.RequireAuth(handlers.CreateEnumerateHandler(storage, partition))
		case "stat":
			handler = auth.RequireAuth(handlers.CreateStatHandler(storage, partition))
		default:
			handler = handlers.CreateGetHandler(storage)
		}
	case "POST":
		switch action {
		case "stat":
			handler = auth.RequireAuth(handlers.CreateStatHandler(storage, partition))
		case "upload":
			handler = auth.RequireAuth(handlers.CreateUploadHandler(storage, partition))
		case "remove":
			// Currently only allows removing from a non-main partition.
			handler = auth.RequireAuth(handlers.CreateRemoveHandler(storage, partition))
		}
	case "PUT": // no longer part of spec
		handler = auth.RequireAuth(handlers.CreateNonStandardPutHandler(storage, partition))
	}
	handler(conn, req)
}

func handleRoot(conn http.ResponseWriter, req *http.Request) {
	configLink := ""
	if *flagUseConfigFiles {
		configLink = "<p>If you're coming from localhost, hit <a href='/setup'>/setup</a>.</p>"
	}
	fmt.Fprintf(conn,
		"<html><body>This is camlistored, a " +
		"<a href='http://camlistore.org'>Camlistore</a> server." +
		"%s</body></html>\n", configLink)
}

func exitFailure(pattern string, args ...interface{}) {
	if !strings.HasSuffix(pattern, "\n") {
		pattern = pattern + "\n"
	}
	fmt.Fprintf(os.Stderr, pattern, args...)
	os.Exit(1)
}

var queuePartitionMap = make(map[string]blobserver.Partition)

func setupMirrorPartitions() {
	queuePartitionMap[""] = mainPartition
	if *flagQueuePartitions == "" {
		return
	}
	for _, partName := range strings.Split(*flagQueuePartitions, ",", -1) {
		if _, dup := queuePartitionMap[partName]; dup {
			log.Fatalf("Duplicate partition in --queue-partitions")
		}
		part := &partitionConfig{name: partName, writable: false, readable: true, queue: true}
		part.urlbase = mainPartition.urlbase + "/partition-" + partName
		mainPartition.mirrors = append(mainPartition.mirrors, part)
	}
}

func main() {
	flag.Parse()

	if *flagUseConfigFiles {
		configFileMain()
		return
	}

	commandLineConfigurationMain()
}

func commandLineConfigurationMain() {
	auth.AccessPassword = os.Getenv("CAMLI_PASSWORD")
	if len(auth.AccessPassword) == 0 {
		exitFailure("No CAMLI_PASSWORD environment variable set.")
	}

	if *flagStorageRoot == "" {
		exitFailure("No storage root specified in --root")
	}

	var err os.Error
	storage, err = localdisk.New(*flagStorageRoot)
	if err != nil {
		exitFailure("Error for --root of %q: %v", *flagStorageRoot, err)
	}

	ws := webserver.New()

	mainPartition.urlbase = ws.BaseURL()
	log.Printf("Base URL is %q", mainPartition.urlbase)
	setupMirrorPartitions() // after mainPartition.urlbase is set

	ws.RegisterPreMux(webserver.HandlerPicker(pickPartitionHandlerMaybe))
	ws.HandleFunc("/", handleRoot)
	ws.HandleFunc("/camli/", handleCamli)

	var (
		myIndexer *mysqlindexer.Indexer
		ownerBlobRef *blobref.BlobRef
	)
	if *flagDevSearch || *flagDevMySql {
		ownerBlobRef = client.SignerPublicKeyBlobref()
		if ownerBlobRef == nil {
			log.Fatalf("Public key not configured.")
		}

		myIndexer = &mysqlindexer.Indexer{
			Host:     "localhost",
			User:     "root",
			Password: "root",
			Database: *flagDatabaseName,
		        OwnerBlobRef: ownerBlobRef,
			KeyFetcher: blobref.NewSerialFetcher(
				blobref.NewConfigDirFetcher(),
				storage),
		}
		if ok, err := myIndexer.IsAlive(); !ok {
			log.Fatalf("Could not connect indexer to MySQL server: %s", err)
		}
	}

	// TODO: temporary
	if *flagDevSearch {
		ws.HandleFunc("/camli/search", func(conn http.ResponseWriter, req *http.Request) {
			handler := auth.RequireAuth(search.CreateHandler(myIndexer, ownerBlobRef))
			handler(conn, req)
		})
	}

	// TODO: temporary
	if *flagDevMySql {
		ws.HandleFunc("/indexer/", makeIndexHandler(myIndexer))
	}

	ws.Handle("/js/", http.FileServer("../../clients/js", "/js/"))
	ws.Serve()
}

func configFileMain() {
	ws := webserver.New()
	ws.HandleFunc("/", handleRoot)
	ws.HandleFunc("/setup", setupHome)
	ws.Serve()
}
