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
	escape "main/utils"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"time"
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
			log.Printf("[ERROR] Failed to obtain form file: %v", err)
			return // http.StatusInternalServerError, "", fmt.Errorf("cannot obtain the uploaded content")
		}
		uploadFileReadeCloser := http.MaxBytesReader(responseWriter, uploadedFile, moLog.MaxUploadSize)
		// MaxBytesReader closes the underlying io.Reader on its Close() is called
		defer uploadFileReadeCloser.Close()

		// Construct path for push API (keywords for search: grafana.com promtail-push-api plaintext payload)
		baseStreams := make(map[string]*string)
		// Read basic label, value pairs from query string
		for label, values := range request.URL.Query() {
			for _, value := range values {
				baseStreams[label] = &value
			}
		}
		filename := uploadedFileInfo.Filename // Additional label
		log.Printf("Filename is %v", filename)
		timestampDate := time.Now().Format(time.DateOnly)
		if filename != "" {
			fileDate := strings.Split(filename[0:8], ".")
			timestampDate = fmt.Sprintf("20%v-%v-%v", fileDate[2], fileDate[0], fileDate[1])
		}

		// Read and unzip file
		zipReader, err := zip.NewReader(uploadedFile, uploadedFileInfo.Size)
		if err != nil {
			log.Printf("[ERROR] Error read archive file %v (error: %v)", filename, err)
			return
		}
		for _, packedFile := range zipReader.File {
			if strings.HasSuffix(packedFile.Name, "Verbose.log") {
				packedFileReadCloser, err := packedFile.Open()
				if err != nil {
					log.Printf("[ERROR] Error unpacked file %v from archive %v (error: %v)", packedFile, filename, err)
					return
				}
				defer packedFileReadCloser.Close()
				packedFileScanner := bufio.NewScanner(packedFileReadCloser)
				packedFileScanner.Split(bufio.ScanLines)
				for packedFileScanner.Scan() {
					rawPushPayload := packedFileScanner.Text()

					// Line sample for custom format
					// 00:09:58:096__FINE_____TAG_AuthManag            |﹏AuthManag <--
					timestampTimeComponents := strings.Split(rawPushPayload[0:12], ":")
					timestampTime := fmt.Sprintf(
						"%v:%v:%v.%v",
						timestampTimeComponents[0],
						timestampTimeComponents[1],
						timestampTimeComponents[2],
						timestampTimeComponents[3],
					)
					logLevel := strings.ReplaceAll(rawPushPayload[14:23], "_", "")
					logSource := strings.ReplaceAll(rawPushPayload[23:48], " ", "")
					_, logTag, logSourceIsTag := strings.Cut(logSource, "_")

					// Push to promtail
					streams := maps.Clone(baseStreams)
					streams["level"] = &logLevel
					if logSourceIsTag {
						streams["tag"] = &logTag
					} else {
						streams["source"] = &logSource
					}

					// Parse time value
					timestampText := fmt.Sprintf("%vT%v000000+03:00", timestampDate, timestampTime)
					timestamp, err := time.Parse(time.RFC3339Nano, timestampText)
					if err != nil {
						log.Printf("[ERROR] Failed parse timestamp %v: %v", timestampText, err)
						return
					}

					// Make post request to promtail
					promtailRequest, err := makePromtailRequest(
						streams,
						timestamp,
						rawPushPayload,
						promtailConfig,
					)
					if err != nil {
						log.Printf("[ERROR] Failed make request: %v", err)
						return
					}

					promtailResponse, err := http.DefaultClient.Do(promtailRequest)
					if err != nil {
						log.Printf("[ERROR] Failed to POST: %v", err)
						return
					}
					defer promtailResponse.Body.Close()
					if promtailResponse.StatusCode != http.StatusNoContent {
						log.Printf("[INFO] status = %d, want = %d", promtailResponse.StatusCode, http.StatusNoContent)
						if ct := promtailResponse.Header.Get("Content-Type"); ct != "application/json" {
							log.Printf("Content-Type = %s, want = \"application/json\"", ct)
							result_body, err := io.ReadAll(promtailResponse.Body)
							if err != nil {
								log.Printf("[ERROR] Failed to read response body: %v", err)
								return
							}
							log.Printf("[INFO] Push result %v", string(result_body[:]))
						} else {
							body, err := io.ReadAll(promtailResponse.Body)
							if err != nil {
								log.Printf("[ERROR] Failed to read response body: %v", err)
								return
							}
							var result SuccessfullyUploadedResult
							if err := json.Unmarshal(body, &result); err != nil {
								log.Printf("[ERROR] Failed to decode response body: %v", err)
								return
							}
							log.Printf("[INFO] Push result as json %v", result)
						}
					}
				}
			}
		}
		return
	}
	responseWriter.WriteHeader(404)
}

func makePromtailRequest(streams map[string]*string, timestamp time.Time, payload string, promtailConfig *MoLogPromtail) (*http.Request, error) {
	requestBodyBuffer := new(bytes.Buffer)
	requestBodyBuffer.WriteString("{\"streams\":[{\"stream\":{")
	lastLabel := len(streams)
	for label, value := range streams {
		requestBodyBuffer.WriteString("\"")
		requestBodyBuffer.WriteString(label)
		requestBodyBuffer.WriteString("\":\"")
		requestBodyBuffer.WriteString(*value)
		requestBodyBuffer.WriteString("\"")
		lastLabel -= 1
		if lastLabel > 0 {
			requestBodyBuffer.WriteString(",")
		}
	}
	requestBodyBuffer.WriteString("},\"values\":[[\"")                                           // TODO: put batch into the request
	requestBodyBuffer.WriteString(fmt.Sprintf("%v%v", timestamp.Unix(), timestamp.Nanosecond())) // TODO: add lead zero to the nanoseconds
	requestBodyBuffer.WriteString("\",")
	requestBodyBuffer.WriteString(escape.JSON(payload))
	requestBodyBuffer.WriteString("]]}]}")

	if promtailConfig.PromtailClientConfig["url"] == nil {
		return nil, fmt.Errorf("EMPTY promtail url settings")
	}

	// TODO: remove debug line fmt.Printf("BODY is %v\n", requestBodyBuffer.String())

	req, err := http.NewRequest(
		"POST",
		fmt.Sprintf("%v", promtailConfig.PromtailClientConfig["url"]),
		requestBodyBuffer,
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json") // TODO: use reference for mime type
	return req, nil
}
