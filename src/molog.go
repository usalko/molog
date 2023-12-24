package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// MoLog Promtail to endpoint config
type MoLog struct {
	Address       string
	TLSCertFile   string
	TLSKeyFile    string
	SourceFile    string
	Promtails     map[string]*MoLogPromtail
	TestUIs       map[string]*string
	MaxUploadSize int64
}

// MoLogPromtail Redis config
type MoLogPromtail struct {
	PromtailClientConfig map[string]interface{}
}

type TemplateInfo struct {
	TestPath, WSURL string
}

type SuccessfullyUploadedResult struct {
	OK   bool   `json:"ok"`
	Path string `json:"path"`
}

var localStatic = false
var testTemplate *template.Template
var rexStatic = regexp.MustCompile(`(.*)(/static/.+(\.[a-z0-9]+))$`)
var mimeTypes = map[string]string{
	".js":   "application/javascript",
	".htm":  "text/html; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".json": "application/json",
	".xml":  "text/xml; charset=utf-8",
	".jpg":  "image/jpeg",
	".png":  "image/png",
	".svg":  "image/svg+xml",
	".gif":  "image/gif",
	".pdf":  "application/pdf",
}

// func parseQueryString(query url.Values, key string, val string) string {
// 	if val == "" {
// 		if values, exists := query[key]; exists && len(values) > 0 {
// 			return values[0]
// 		}
// 	}
// 	return val
// }

// Start start websocket and start consuming from Redis stream(s)
func (moLog *MoLog) Start() error {
	if moLog.TLSCertFile != "" {
		return http.ListenAndServeTLS(moLog.Address, moLog.TLSCertFile, moLog.TLSKeyFile, moLog)
	}
	return http.ListenAndServe(moLog.Address, moLog)
}

func (moLog *MoLog) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	subMatch := rexStatic.FindStringSubmatch(request.URL.Path)
	if len(subMatch) > 0 {
		// serve static files
		testUIPath := subMatch[1]
		if testUIPath == "" {
			testUIPath = "/"
		}
		if _, exists := moLog.TestUIs[testUIPath]; exists {
			if payload, err := FSByte(localStatic, subMatch[2]); err == nil {
				var mime = "application/octet-stream"
				if m, ok := mimeTypes[subMatch[3]]; ok {
					mime = m
				}
				responseWriter.Header().Add("Content-Type", mime)
				responseWriter.Header().Add("Content-Length", fmt.Sprintf("%d", len(payload)))
				responseWriter.Write(payload)
				return
			}
		}
	} else if uploadPath, exists := moLog.TestUIs[request.URL.Path]; exists {
		html, err := FSString(localStatic, "/static/test.html")
		if err == nil {
			uploadURL := "http://" + request.Host + *uploadPath
			if moLog.TLSCertFile != "" {
				uploadURL = "https://" + request.Host + *uploadPath
			}
			if testTemplate == nil || localStatic {
				testTemplate = template.Must(template.New("").Parse(html))
			}
			testTemplate.Execute(responseWriter, TemplateInfo{strings.TrimRight(request.URL.Path, "/"), uploadURL})
		}
		return
	} else if promtailConfig, exists := moLog.Promtails[request.URL.Path]; exists {
		// TODO: check method (only PUT and POST allowed)
		// Upload file
		uploadedFile, uploadedFileInfo, err := request.FormFile("file")
		if err != nil {
			log.Printf("failed to obtain form file: %v", err)
			return // http.StatusInternalServerError, "", fmt.Errorf("cannot obtain the uploaded content")
		}
		uploadFileReadeCloser := http.MaxBytesReader(responseWriter, uploadedFile, moLog.MaxUploadSize)
		// MaxBytesReader closes the underlying io.Reader on its Close() is called
		defer uploadFileReadeCloser.Close()

		// Construct path for push API (keywords for search: grafana.com promtail-push-api plaintext payload)
		var basePushPath strings.Builder
		// Read basic label, value pairs from query string
		for label, values := range request.URL.Query() {
			for _, value := range values {
				if basePushPath.Len() > 0 {
					basePushPath.WriteByte('/')
				}
				basePushPath.WriteString(url.QueryEscape(label))
				basePushPath.WriteByte('/')
				basePushPath.WriteString(url.QueryEscape(value))
			}
		}
		filename := uploadedFileInfo.Filename // Additional label
		log.Printf("Filename is %v", filename)
		timestampDate := "2023-12-24"
		if filename != "" {
			timestampDate = "2023-12-23"
		}

		// Read and unzip file
		zipReader, err := zip.NewReader(uploadedFile, uploadedFileInfo.Size)
		if err != nil {
			log.Fatalf("Error read archive file %v (error: %v)", filename, err)
			return
		}
		for _, packedFile := range zipReader.File {
			if strings.HasSuffix(packedFile.Name, "Verbose.log") {
				packedFileReadCloser, err := packedFile.Open()
				if err != nil {
					log.Fatalf("Error unpacked file %v from archive %v (error: %v)", packedFile, filename, err)
					return
				}
				defer packedFileReadCloser.Close()
				packedFileScanner := bufio.NewScanner(packedFileReadCloser)
				packedFileScanner.Split(bufio.ScanLines)
				for packedFileScanner.Scan() {
					rawPushPayload := packedFileScanner.Text()

					// Line sample for custom format
					// 00:09:58:096__FINE_____TAG_AuthManag            |﹏AuthManag <--
					timestampTime := rawPushPayload[0:12]
					logLevel := strings.ReplaceAll(rawPushPayload[14:23], "_", "")
					logSource := strings.ReplaceAll(rawPushPayload[23:48], " ", "")
					_, logTag, logSourceIsTag := strings.Cut(logSource, "_")

					// Push to promtail
					var pushPath strings.Builder
					pushPath.WriteString(pushPath.String())
					pushPath.WriteString("/")
					pushPath.WriteString("level")
					pushPath.WriteString("/")
					pushPath.WriteString(logLevel)
					if logSourceIsTag {
						pushPath.WriteString("/")
						pushPath.WriteString("tag")
						pushPath.WriteString("/")
						pushPath.WriteString(logTag)
					} else {
						pushPath.WriteString("/")
						pushPath.WriteString("source")
						pushPath.WriteString("/")
						pushPath.WriteString(logSource)
					}

					// Make post request to promtail
					promtailRequest, err := makePromtailRequest(
						pushPath.String(),
						fmt.Sprintf("%vT%v+03:00", timestampDate, timestampTime),
						rawPushPayload,
						promtailConfig,
					)
					if err != nil {
						log.Fatalf("Failed make request: %v", err)
						return
					}

					promtailResponse, err := http.DefaultClient.Do(promtailRequest)
					if err != nil {
						log.Fatalf("Failed to POST: %v", err)
						return
					}
					defer promtailResponse.Body.Close()
					if promtailResponse.StatusCode != http.StatusCreated {
						log.Printf("status = %d, want = %d", promtailResponse.StatusCode, http.StatusCreated)
					}
					if ct := promtailResponse.Header.Get("Content-Type"); ct != "application/json" {
						log.Printf("Content-Type = %s, want = \"application/json\"", ct)
					}
					body, err := io.ReadAll(promtailResponse.Body)
					if err != nil {
						log.Fatalf("failed to read response body: %v", err)
						return
					}
					var result SuccessfullyUploadedResult
					if err := json.Unmarshal(body, &result); err != nil {
						log.Fatalf("failed to decode response body: %v", err)
						return
					}
					log.Printf("Push result %v", result)

				}
			}
		}

		// // Context
		// ctx := context.Background()

		// // Make sure to read client message and react on close/error
		// chClose := make(chan bool)

		// go func() {
		// 	defer wsConnection.Close()

		// 	for {
		// 		_, _, err := wsutil.ReadClientData(wsConnection)
		// 		if err != nil {
		// 			// handle error
		// 			chClose <- true
		// 			if !strings.HasPrefix(err.Error(), "websocket: close") {
		// 				log.Printf("WebSocket read error: %v\n", err)
		// 			}
		// 			return
		// 		}
		// 	}
		// }()

		// // Subscribe client to the stream messages
		// chStream := make(chan redis.XStream)

		// // Subscribe client to the errors
		// chError := make(chan error)

		// go func() {
		// 	defer client.Close()

		// 	if len(streams) == 0 {
		// 		chError <- errors.New("no streams for listening")
		// 		return
		// 	}

		// 	// Filter for existing streams
		// 	streamsRequest := make([]string, 0)
		// 	for _, stream := range streams {
		// 		existedStreams, _, err := client.Scan(ctx, 0, stream, -1).Result()
		// 		if err != nil {
		// 			fmt.Printf("Can't read streams %v: %v\n", streams, err)
		// 			chError <- err
		// 			return
		// 		}
		// 		if len(existedStreams) == 0 {
		// 			// Fallback to exists query
		// 			streamKeyExists, err := client.Exists(ctx, stream).Result()
		// 			if err != nil {
		// 				fmt.Printf("Can't request exist key %v: %v\n", streams, err)
		// 				chError <- err
		// 				return
		// 			}
		// 			if streamKeyExists != 0 {
		// 				existedStreams = []string{stream}
		// 			}
		// 		}

		// 		fmt.Printf("Success scan the stream: %v\nExisted streams: %v\n", stream, existedStreams)
		// 		streamsRequest = append(streamsRequest, existedStreams...)
		// 	}

		// 	if len(streamsRequest) == 0 {
		// 		errorDescription := fmt.Sprintf("The streams: %v not found in redis\n", streams)
		// 		chError <- errors.New(errorDescription)
		// 		return
		// 	}

		// 	idsOffset := len(streamsRequest)
		// 	ids := make([]string, idsOffset)
		// 	for i := 0; i < len(streamsRequest); i++ {
		// 		ids[i] = "0" // '$' argument see redis documentation
		// 	}
		// 	streamsRequest = append(streamsRequest, ids...)
		// 	for {
		// 		xStreams, err := client.XRead(ctx, &redis.XReadArgs{
		// 			Streams: streamsRequest,
		// 			Block:   0,
		// 		}).Result()
		// 		if err != nil {
		// 			fmt.Printf("Can't read streams %v: %v\n", streams, err)
		// 			chError <- err
		// 			return
		// 		}
		// 		for _, xStream := range xStreams {
		// 			chStream <- xStream
		// 		}
		// 	}
		// }()

		// log.Printf("Websocket opened %s\n", r.Host)
		// running := true
		// // Keep reading and sending messages
		// for running {
		// 	select {
		// 	// Exit if websocket read fails
		// 	case <-chClose:
		// 		running = false
		// 	case ev := <-chError:
		// 		switch e := ev.(type) {
		// 		case redis.Error:
		// 			if e.Error() == "redis: nil" {
		// 				log.Printf("%% Error: %v perhaps stream(s) %v didn't exists\n", e, streams)
		// 			} else {
		// 				log.Printf("%% Error: %v\n", e)
		// 			}
		// 			err = wsConnection.Close()
		// 			if err != nil {
		// 				log.Printf("Error while closing WebSocket: %v\n", e)
		// 			}
		// 			running = false
		// 		default:
		// 			log.Printf("Error type: %v", ev)
		// 			err = wsConnection.Close()
		// 			if err != nil {
		// 				log.Printf("Error while closing WebSocket: %v\n", e)
		// 			}
		// 			running = false
		// 		}
		// 	case stream := <-chStream:
		// 		values, jsonErrors := JSONBytesMake(stream.Messages, promtailConfig.MessageType)
		// 		if jsonErrors == nil {
		// 			err = wsutil.WriteServerMessage(wsConnection, ws.OpBinary, values)
		// 		} else {
		// 			err = wsutil.WriteServerMessage(wsConnection, ws.OpBinary, []byte(jsonErrors.Error()))
		// 		}

		// 		if err != nil {
		// 			// handle error
		// 			chClose <- true
		// 			log.Printf("WebSocket write error: %v (%v)\n", err, stream)
		// 			running = false
		// 		} else {
		// 			for _, xMessage := range stream.Messages {
		// 				// TODO: call single command with multiply message ids
		// 				client.XDel(ctx, stream.Stream, xMessage.ID)
		// 			}
		// 		}
		// 	}
		// }
		// log.Printf("Websocket closed %s\n", r.Host)
		return
	}
	responseWriter.WriteHeader(404)
}

func makePromtailRequest(pushPath string, timestamp string, payload string, promtailConfig *MoLogPromtail) (*http.Request, error) {
	requestBodyBuffer := new(bytes.Buffer)
	requestBodyBuffer.WriteString(payload)

	if promtailConfig.PromtailClientConfig["url"] == nil {
		return nil, fmt.Errorf("EMPTY promtail url settings")
	}

	req, err := http.NewRequest(
		"POST",
		fmt.Sprintf(
			"%v%v?timestamp=%v",
			fmt.Sprintf("%v", promtailConfig.PromtailClientConfig["url"]),
			pushPath,
			timestamp,
		),
		requestBodyBuffer,
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "plain/text") // TODO: use reference for mime type
	return req, nil
}
