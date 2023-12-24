package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	yaml "gopkg.in/yaml.v2"
)

const initConfig = `schema.version: "1.0"
# tls.cert.file: my-domain.crt
# tls.key.file: my-domain.key
promtail.to.endpoint:
  - promtail.client.config:
      # FIXME: https://github.com/edenhill/librdredis/blob/master/CONFIGURATION.md#global-configuration-properties
      url: promtail:6379
	address: :8804
	# promtail.default.label: label1
	# default maximum upload size is 10M
	max.upload.size: 10485760
`

// ConfigMoLog Redis to upload YAML
type ConfigMoLog struct {
	PromtailClientConfig map[string]interface{} `yaml:"promtail.client.config"`
	Address              string                 `yaml:"address"`
	MaxUploadSize        int64                  `yaml:"max.upload.size"`
	EndpointPrefix       string                 `yaml:"endpoint.prefix"`
	EndpointTest         string                 `yaml:"endpoint.test"`
	EndpointUpload       string                 `yaml:"endpoint.upload"`
	Compression          bool                   `yaml:"compression"`
}

// Config YAML config file
type Config struct {
	SchemaVersion string        `yaml:"schema.version"`
	TLSCertFile   string        `yaml:"tls.cert.file"`
	TLSKeyFile    string        `yaml:"tls.key.file"`
	ConfigMoLogs  []ConfigMoLog `yaml:"promtail.to.endpoint"`
}

// ReadMoLog read config file and returns collection of MoLog
func ReadMoLog(filename string) []*MoLog {
	fileContent, err := os.ReadFile(filename)
	if err != nil {
		absPath, _ := filepath.Abs(filename)
		log.Fatalf("Error while reading %v file: \n%v ", absPath, err)
	}
	log.Printf("%s\n%s", filename, string(fileContent))
	var config Config
	err = yaml.Unmarshal(fileContent, &config)
	if err != nil {
		log.Fatalf("Unmarshal: %v", err)
	}

	certFile := ""
	keyFile := ""
	if config.TLSCertFile != "" && config.TLSKeyFile == "" || config.TLSCertFile == "" && config.TLSKeyFile != "" {
		panic(fmt.Sprintf("Both certificate and key file must be defined %v", "."))
	} else if config.TLSCertFile != "" {
		if _, err := os.Stat(config.TLSCertFile); err == nil {
			if _, err := os.Stat(config.TLSKeyFile); err == nil {
				keyFile = config.TLSKeyFile
				certFile = config.TLSCertFile
			} else {
				panic(fmt.Sprintf("key file %s does not exist", config.TLSKeyFile))
			}
		} else {
			panic(fmt.Sprintf("certificate file %s does not exist", config.TLSKeyFile))
		}
	}
	moLogMap := make(map[string]*MoLog)
	for _, moLogConfig := range config.ConfigMoLogs {
		var moLog *MoLog
		var exists bool
		if moLog, exists = moLogMap[moLogConfig.Address]; !exists {

			// Redefine default port to 8804
			if moLogConfig.Address == "" {
				moLogConfig.Address = ":8804"
			}

			moLog = &MoLog{
				Address:       moLogConfig.Address,
				TLSCertFile:   certFile,
				TLSKeyFile:    keyFile,
				SourceFile:    filename,
				Promtails:     make(map[string]*MoLogPromtail),
				TestUIs:       make(map[string]*string),
				MaxUploadSize: moLogConfig.MaxUploadSize,
			}
			moLogMap[moLogConfig.Address] = moLog
		}
		testPath := moLogConfig.EndpointTest
		uploadPath := moLogConfig.EndpointUpload
		if testPath == "" && uploadPath == "" {
			testPath = "test"
		}
		if moLogConfig.EndpointPrefix != "" {
			testPath = moLogConfig.EndpointPrefix + "/" + testPath
			uploadPath = moLogConfig.EndpointPrefix + "/" + uploadPath
		}
		testPath = "/" + strings.TrimRight(testPath, "/")
		uploadPath = "/" + strings.TrimRight(uploadPath, "/")

		if testPath == uploadPath {
			panic(fmt.Sprintf("test path and upload path can't be same [%s]", moLogConfig.EndpointTest))
		}
		if moLogConfig.PromtailClientConfig["url"] == "" {
			panic(fmt.Sprintf("Promtail url must be defined for promtail.to.endpoint address [%s]", moLogConfig.Address))
		}
		if _, exists := moLog.TestUIs[testPath]; exists {
			panic(fmt.Sprintf("test path [%s] already defined", testPath))
		}
		if _, exists := moLog.Promtails[testPath]; exists {
			panic(fmt.Sprintf("test path [%s] already defined as upload path", testPath))
		}
		if _, exists := moLog.Promtails[uploadPath]; exists {
			panic(fmt.Sprintf("upload path [%s] already defined", uploadPath))
		}
		if _, exists := moLog.TestUIs[uploadPath]; exists {
			panic(fmt.Sprintf("upload path [%s] already defined as test path", uploadPath))
		}
		moLog.TestUIs[testPath] = &uploadPath
		moLog.Promtails[uploadPath] = &MoLogPromtail{
			PromtailClientConfig: moLogConfig.PromtailClientConfig,
		}
	}
	moLogSlice := make([]*MoLog, len(moLogMap))
	i := 0
	for _, moLog := range moLogMap {
		moLogSlice[i] = moLog
		i++
	}
	return moLogSlice
}
