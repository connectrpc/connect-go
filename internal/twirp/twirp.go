package twirp

// Status is the structure of all Twirp error responses.
type Status struct {
	Code     string            `json:"code"`
	Message  string            `json:"msg"`
	Metadata map[string]string `json:"metadata,omitempty"` // unused
}
