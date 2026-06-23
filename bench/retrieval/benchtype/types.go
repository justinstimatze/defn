// Package benchtype defines shared types for the cross-system context retrieval benchmark.
// This is a leaf package: it imports nothing from knowing's internal packages,
// allowing both adapters and metrics to import it without cycles.
package benchtype

// Task defines a benchmark evaluation task with ground truth.
type Task struct {
	ID          string   `yaml:"id"`
	Repo        string   `yaml:"repo"`
	Tier        string   `yaml:"tier"`         // easy, medium, hard
	Description string   `yaml:"description"`  // the task query given to each system
	GroundTruth []string `yaml:"ground_truth"` // qualified symbol names
	Tags        []string `yaml:"tags"`
	Notes       string   `yaml:"notes"`
	Source      string   `yaml:"source"` // swe-bench, manual, synthetic
}

// RetrievalResult is what an adapter returns for a single task.
type RetrievalResult struct {
	System      string            `json:"system"`
	TaskID      string            `json:"task_id"`
	Symbols     []RetrievedSymbol `json:"symbols"`
	TokensUsed  int               `json:"tokens_used"`
	LatencyMs   int64             `json:"latency_ms"`
	IndexTimeMs int64             `json:"index_time_ms,omitempty"`
	RawOutput   string            `json:"raw_output,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// RetrievedSymbol is a single symbol returned by a system.
type RetrievedSymbol struct {
	QualifiedName string  `json:"qualified_name"`
	Normalized    string  `json:"normalized"`
	Score         float64 `json:"score,omitempty"`
	Rank          int     `json:"rank"`
	FilePath      string  `json:"file_path,omitempty"`
	Kind          string  `json:"kind,omitempty"`
}

// MetricResult holds computed metrics for one system on one task.
type MetricResult struct {
	System          string  `json:"system"`
	TaskID          string  `json:"task_id"`
	PrecisionAt5    float64 `json:"precision_at_5"`
	PrecisionAt10   float64 `json:"precision_at_10"`
	PrecisionAt20   float64 `json:"precision_at_20"`
	RecallAt5       float64 `json:"recall_at_5"`
	RecallAt10      float64 `json:"recall_at_10"`
	RecallAt20      float64 `json:"recall_at_20"`
	NDCGAt10        float64 `json:"ndcg_at_10"`
	MRR             float64 `json:"mrr"`
	F1At10          float64 `json:"f1_at_10"`
	TokenEfficiency float64 `json:"token_efficiency"`
	TokensUsed      int     `json:"tokens_used"`
	LatencyMs       int64   `json:"latency_ms"`
}

// AggregateResult holds per-system aggregate metrics across all tasks.
type AggregateResult struct {
	System            string  `json:"system"`
	MeanPrecisionAt10 float64 `json:"mean_precision_at_10"`
	MeanRecallAt10    float64 `json:"mean_recall_at_10"`
	MeanNDCGAt10      float64 `json:"mean_ndcg_at_10"`
	MeanMRR           float64 `json:"mean_mrr"`
	MeanF1At10        float64 `json:"mean_f1_at_10"`
	MeanTokenEff      float64 `json:"mean_token_efficiency"`
	MedianLatencyMs   int64   `json:"median_latency_ms"`
	TaskCount         int     `json:"task_count"`
	FailureCount      int     `json:"failure_count"`
}

// Adapter is the interface each benchmarked system implements.
type Adapter interface {
	Name() string
	Index(repoPath string) (indexTimeMs int64, err error)
	Retrieve(repoPath string, task Task, tokenBudget int) (RetrievalResult, error)
	SupportsLearning() bool
	RecordFeedback(repoPath string, task Task, relevantSymbols []string) error
	Reset(repoPath string) error
}
