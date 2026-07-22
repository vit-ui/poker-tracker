package sheets

// A minimal REST transport for the seven Google API endpoints this app
// actually calls, replacing the official google.golang.org/api client. The
// official client transitively linked gRPC, protobuf, OpenTelemetry, and the
// cloud auth stack — none of it used by these plain-REST calls — and cost
// ~12 MB of the release binary (20.3 MB with it, ~8 MB without). Wire shapes
// (JSON bodies, query parameters, URL layout) match what the official client
// sent and received, so cell encoding/decoding semantics are unchanged:
// json.Unmarshal into [][]interface{} yields float64/string exactly as
// before. Auth is the service account's JWT flow via golang.org/x/oauth2,
// whose http.Client refreshes tokens automatically.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"golang.org/x/oauth2/google"
)

const (
	sheetsBaseURL = "https://sheets.googleapis.com/v4/spreadsheets/"
	driveFilesURL = "https://www.googleapis.com/drive/v3/files"

	spreadsheetsScope          = "https://www.googleapis.com/auth/spreadsheets"
	driveMetadataReadonlyScope = "https://www.googleapis.com/auth/drive.metadata.readonly"
)

// restService issues authenticated Google API requests. hc comes from the
// service account's JWT config and refreshes its token automatically.
// baseURL and driveURL are the API roots — always the real Google endpoints
// in production (see newRESTService), overridden only by the package's own
// tests, which point them at an httptest server standing in for Google so
// the read/write/fallback code paths run against real HTTP without real
// credentials.
type restService struct {
	hc       *http.Client
	baseURL  string
	driveURL string
}

// newRESTService builds a transport from the service account key JSON,
// authorized for the given scope.
func newRESTService(serviceAccountJSON []byte, scope string) (*restService, error) {
	conf, err := google.JWTConfigFromJSON(serviceAccountJSON, scope)
	if err != nil {
		return nil, fmt.Errorf("parsing credentials: %w", err)
	}
	return &restService{hc: conf.Client(context.Background()), baseURL: sheetsBaseURL, driveURL: driveFilesURL}, nil
}

// valueRange is the request/response payload for every values endpoint —
// the same JSON shape the official client used on the wire.
type valueRange struct {
	Range  string          `json:"range,omitempty"`
	Values [][]interface{} `json:"values,omitempty"`
}

// doJSON performs one API call: marshals body (if non-nil), sends, checks
// for a 2xx status, and decodes the response into out (if non-nil). Non-2xx
// responses return an error carrying the status and the start of the body —
// enough to tell an unshared sheet (403) from a bad ID (404) from a
// transient server error (5xx, which the write-ahead fallback catches).
func (s *restService) doJSON(method, reqURL string, body, out interface{}) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, reqURL, reqBody)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("google api: %s: %s", resp.Status, snippet)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

// valuesURL builds the .../values/{range} URL. Ranges contain spaces (tab
// names like "Cash Sessions") and must be path-escaped; suffix is "" or an
// API verb like ":append".
func (s *restService) valuesURL(spreadsheetID, rng, suffix, query string) string {
	return s.baseURL + spreadsheetID + "/values/" + url.PathEscape(rng) + suffix + "?" + query
}

// valuesGet reads a range with UNFORMATTED_VALUE rendering — numbers come
// back as JSON numbers (float64), text as strings, exactly what parseFloat/
// parseInt/fmt.Sprint downstream expect. Trailing empty cells and rows are
// omitted by the API, same as with the official client.
func (s *restService) valuesGet(spreadsheetID, rng string) (valueRange, error) {
	var out valueRange
	err := s.doJSON(http.MethodGet, s.valuesURL(spreadsheetID, rng, "", "valueRenderOption=UNFORMATTED_VALUE"), nil, &out)
	return out, err
}

// valuesUpdate overwrites a range with RAW input (no cell-format parsing).
func (s *restService) valuesUpdate(spreadsheetID, rng string, values [][]interface{}) error {
	return s.doJSON(http.MethodPut, s.valuesURL(spreadsheetID, rng, "", "valueInputOption=RAW"),
		valueRange{Values: values}, nil)
}

// valuesAppend appends rows after the last data row of the table containing
// rng, with RAW input.
func (s *restService) valuesAppend(spreadsheetID, rng string, values [][]interface{}) error {
	return s.doJSON(http.MethodPost, s.valuesURL(spreadsheetID, rng, ":append", "valueInputOption=RAW"),
		valueRange{Values: values}, nil)
}

// valuesClear deletes the values in a range, leaving the cells empty. Used to
// roll back a partially-written migration import.
func (s *restService) valuesClear(spreadsheetID, rng string) error {
	return s.doJSON(http.MethodPost, s.valuesURL(spreadsheetID, rng, ":clear", ""), struct{}{}, nil)
}

// valuesBatchUpdate writes several disjoint ranges in one call.
func (s *restService) valuesBatchUpdate(spreadsheetID string, data []valueRange) error {
	body := struct {
		ValueInputOption string       `json:"valueInputOption"`
		Data             []valueRange `json:"data"`
	}{"RAW", data}
	return s.doJSON(http.MethodPost, s.baseURL+spreadsheetID+"/values:batchUpdate", body, nil)
}

// spreadsheetTabTitles returns the titles of every tab in the spreadsheet.
// Also NewClient's reachability check: an unshared or nonexistent sheet
// fails here first.
func (s *restService) spreadsheetTabTitles(spreadsheetID string) ([]string, error) {
	var out struct {
		Sheets []struct {
			Properties struct {
				Title string `json:"title"`
			} `json:"properties"`
		} `json:"sheets"`
	}
	if err := s.doJSON(http.MethodGet, s.baseURL+spreadsheetID+"?fields=sheets.properties.title", nil, &out); err != nil {
		return nil, err
	}
	titles := make([]string, len(out.Sheets))
	for i, sh := range out.Sheets {
		titles[i] = sh.Properties.Title
	}
	return titles, nil
}

// addSheet creates a new tab with the given title.
func (s *restService) addSheet(spreadsheetID, title string) error {
	body := map[string]interface{}{
		"requests": []map[string]interface{}{
			{"addSheet": map[string]interface{}{
				"properties": map[string]string{"title": title},
			}},
		},
	}
	return s.doJSON(http.MethodPost, s.baseURL+spreadsheetID+":batchUpdate", body, nil)
}

// driveFindSpreadsheets returns the IDs of every spreadsheet the service
// account can see with exactly the given name. Used only by ResolveSheetID.
func (s *restService) driveFindSpreadsheets(name string) ([]string, error) {
	q := url.Values{
		"q":      {fmt.Sprintf("name = '%s' and mimeType = 'application/vnd.google-apps.spreadsheet'", name)},
		"fields": {"files(id)"},
	}
	var out struct {
		Files []struct {
			ID string `json:"id"`
		} `json:"files"`
	}
	if err := s.doJSON(http.MethodGet, s.driveURL+"?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, len(out.Files))
	for i, f := range out.Files {
		ids[i] = f.ID
	}
	return ids, nil
}
