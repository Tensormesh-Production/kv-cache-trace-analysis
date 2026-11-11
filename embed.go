// Package kit holds the manifests and the collector configuration that
// tm-trace deploys. They are the files that the README steps use.
package kit

import _ "embed"

// CollectorManifest is the trace collector for Kubernetes.
//
//go:embed deploy/collector/trace-collector.yaml
var CollectorManifest []byte

// AnalyzerManifests are the analyzer Jobs, by trace format.
var AnalyzerManifests = map[string][]byte{
	"lmcache": analyzerLMCache,
	"dynamo":  analyzerDynamo,
	"custom":  analyzerCustom,
}

//go:embed deploy/analyzer/analyzer-lmcache.yaml
var analyzerLMCache []byte

//go:embed deploy/analyzer/analyzer-dynamo.yaml
var analyzerDynamo []byte

//go:embed deploy/analyzer/analyzer-custom.yaml
var analyzerCustom []byte

// DockerCollectorConfig is the trace collector configuration for Docker.
//
//go:embed deploy/docker/collector-config.yaml
var DockerCollectorConfig []byte
