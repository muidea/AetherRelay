package modeltrace

type modelDefinition struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Family      string    `json:"family"`
	FamilyName  string    `json:"family_name"`
	Counts      []float64 `json:"counts"`
}
type artifactDefinition struct {
	Mean         []float64     `json:"feature_mean"`
	Scale        []float64     `json:"feature_scale"`
	Basis        [][]float64   `json:"nuisance_basis"`
	Centroids    [][]float64   `json:"centroids"`
	Environments [][][]float64 `json:"environment_centroids"`
	Weight       float64       `json:"weight"`
}
type calibrationDefinition struct {
	Beta     float64 `json:"beta"`
	Accuracy float64 `json:"cv_accuracy"`
}
type bank struct {
	Schema string            `json:"schema"`
	Models []modelDefinition `json:"models"`
	Robust struct {
		Order     []string            `json:"model_order"`
		Hellinger artifactDefinition  `json:"hellinger"`
		Ordered   *artifactDefinition `json:"ordered_blocks"`
	} `json:"robust"`
	Calibration map[string]calibrationDefinition `json:"calibration"`
}
type Output struct {
	Text          string `json:"text"`
	ExpectedCount int    `json:"expected_count"`
}
type Diagnostic struct {
	Index    int  `json:"index"`
	Parsed   int  `json:"parsed_numbers"`
	Minimum  int  `json:"minimum_numbers"`
	Accepted bool `json:"accepted"`
}
type Candidate struct {
	Model       string  `json:"model"`
	DisplayName string  `json:"display_name"`
	Probability float64 `json:"probability"`
	Similarity  float64 `json:"profile_similarity"`
	Score       float64 `json:"score"`
	Family      string  `json:"family"`
	FamilyName  string  `json:"family_name"`
	Conditional float64 `json:"conditional_probability"`
}
type Family struct {
	Family      string  `json:"family"`
	DisplayName string  `json:"display_name"`
	Probability float64 `json:"probability"`
}
type Analysis struct {
	Prediction     string       `json:"prediction"`
	PredictionName string       `json:"prediction_name"`
	Probability    float64      `json:"probability"`
	Used           int          `json:"used_outputs"`
	Results        []Candidate  `json:"results"`
	Diagnostics    []Diagnostic `json:"diagnostics"`
	Calibration    struct {
		Queries  string  `json:"queries"`
		Beta     float64 `json:"beta"`
		Accuracy float64 `json:"cv_accuracy"`
	} `json:"calibration"`
	FamilyPrediction  string   `json:"family_prediction"`
	FamilyName        string   `json:"family_prediction_name"`
	FamilyProbability float64  `json:"family_probability"`
	Families          []Family `json:"family_probabilities"`
	Method            string   `json:"method"`
}
