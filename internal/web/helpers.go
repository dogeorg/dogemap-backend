package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// sendOptions sends a response to an OPTIONS request.
func sendOptions(w http.ResponseWriter, r *http.Request, options string) {
	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("Allow", options)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.Header().Set("Allow", options)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// sendJson sends a JSON response to a web request.
func sendJson(w http.ResponseWriter, payload any, options string) {
	bytes, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, fmt.Sprintf("error encoding JSON: %s", err.Error()), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private; max-age=0")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(bytes)))
	w.Header().Set("Allow", options)
	w.Write(bytes)
}

type WebError struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

// sendError sends a json error response to a web request.
func sendError(w http.ResponseWriter, statusCode int, code string, reason string, options string) {
	bytes, err := json.Marshal(WebError{Error: code, Reason: reason})
	if err != nil {
		bytes = []byte(fmt.Sprintf("{\"error\":\"json\",\"reason\":\"encoding JSON: %s\"}", err.Error()))
		statusCode = http.StatusInternalServerError
	}
	w.Header().Set("Cache-Control", "private; max-age=0")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(bytes)))
	w.Header().Set("Allow", options)
	w.WriteHeader(statusCode)
	w.Write(bytes)
}

// fetch Json from an http endpoint, optionally posting json.
func fetchJson(url string, postData any, result any) (err error) {
	var res *http.Response
	if postData != nil {
		// either JSON-able postData or pre-encoded payload.
		var payload []byte
		if p, ok := postData.([]byte); ok {
			payload = p
		} else {
			payload, err = json.Marshal(postData)
			if err != nil {
				return fmt.Errorf("fetch: %v: json decode: %v", url, err)
			}
		}
		res, err = http.Post(url, "application/json", bytes.NewReader(payload))
	} else {
		res, err = http.Get(url)
	}
	if err != nil {
		return fmt.Errorf("fetch: %v: %v", url, err)
	}
	defer res.Body.Close() // ensure body is closed
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch: %v: status %v", url, res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("fetch: %v: %v", url, err)
	}
	err = json.Unmarshal(body, result)
	if err != nil {
		return fmt.Errorf("fetch: %v: json decode: %v", url, err)
	}
	return nil
}
