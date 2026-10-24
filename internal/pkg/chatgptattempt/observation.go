// Package chatgptattempt holds credential-free text attempt observations.
package chatgptattempt

type Observation struct {
	Index             int    `json:"index"`
	AccountID         string `json:"account_id,omitempty"`
	AccountLabel      string `json:"account_label,omitempty"`
	Operation         string `json:"operation,omitempty"`
	UpstreamStatus    int    `json:"upstream_status,omitempty"`
	FailureClass      string `json:"failure_class,omitempty"`
	GenerationNotSent bool   `json:"generation_not_sent"`
	Success           bool   `json:"success"`
}
