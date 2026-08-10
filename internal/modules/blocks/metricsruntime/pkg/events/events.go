package events

import (
	"time"

	"aetherrelay/internal/pkg/aetherrelaymetrics"
)

const (
	TopicAcquire        = "aetherrelay.metrics.command.acquire"
	TopicRecord         = "aetherrelay.metrics.event.record"
	TopicPrometheus     = "aetherrelay.metrics.command.prometheus"
	TopicStats          = "aetherrelay.metrics.command.stats"
	TopicProviderHealth = "aetherrelay.metrics.command.provider_health"
	TopicProviderModel  = "aetherrelay.metrics.command.provider_model_health"
	TopicResetHealth    = "aetherrelay.metrics.command.reset_provider_health"
)

type AcquireCommand struct{}
type AcquireResult struct{}

type RecordKind string

const (
	ReserveModels        RecordKind = "reserve_models"
	ClientUsage          RecordKind = "client_usage"
	UsageStoreWriteError RecordKind = "usage_store_write_error"
	UsageStoreQuery      RecordKind = "usage_store_query"
	UsageStoreRecovered  RecordKind = "usage_store_recovered"
	UsageStoreHealthy    RecordKind = "usage_store_healthy"
	RequestPlan          RecordKind = "request_plan"
	Conversion           RecordKind = "conversion"
	Tokens               RecordKind = "tokens"
	UpstreamAttempt      RecordKind = "upstream_attempt"
	UpstreamError        RecordKind = "upstream_error"
)

type RecordCommand struct {
	Kind                RecordKind
	Provider            string
	Model               string
	Models              []string
	APIKeyID            string
	Route               string
	Outcome             string
	ClientEndpoint      string
	ClientProtocol      string
	UpstreamProtocol    string
	UpstreamEndpoint    string
	ConversionMode      string
	ConversionLevel     int
	UpstreamStatus      int
	Degraded            bool
	Estimated           bool
	IgnoredFeatures     []string
	UnsupportedFeatures []string
	Phase               string
	AttemptKind         metrics.AttemptLatencyKind
	Status              int
	Input               int
	Output              int
	Cached              int
	CacheCreation       int
	Count               int64
	Duration            time.Duration
	Healthy             bool
	Failed              bool
}

type PrometheusCommand struct{}
type StatsCommand struct{}
type BytesResult struct{ Data []byte }

type ProviderHealthCommand struct{}
type ProviderHealthResult struct {
	Values map[string]metrics.StatsProviderHealth
}

type ProviderModelHealthCommand struct{ Provider, Model string }
type ProviderModelHealthResult struct {
	Value metrics.StatsProviderHealth
	Found bool
}

type ResetProviderHealthCommand struct{ Provider string }
type ResetProviderHealthResult struct{}
