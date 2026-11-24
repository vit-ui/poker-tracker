package sheets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/oauth2/google"

	"github.com/vit-ui/self-tracker/internal/money"
)

const (
	sheetsBaseURL = "https://sheets.googleapis.com/v4/spreadsheets/"
	driveFilesURL = "https://www.googleapis.com/drive/v3/files"

	spreadsheetsScope          = "https://www.googleapis.com/auth/spreadsheets"
	driveMetadataReadonlyScope = "https://www.googleapis.com/auth/drive.metadata.readonly"

	sheetsRateInterval = 1200 * time.Millisecond
	sheetsRateBurst    = 5
)

type restService struct {
	hc       *http.Client
	baseURL  string
	driveURL string

	healthMu  sync.Mutex
	lastErr   error
	downSince time.Time

	rateMu       sync.Mutex
	rateInterval time.Duration
	rateBurst    int
	rateTokens   float64
	rateLast     time.Time
}

func newRESTService(serviceAccountJSON []byte, scope string) (*restService, error) {
	conf, err := google.JWTConfigFromJSON(serviceAccountJSON, scope)
	if err != nil {
		return nil, fmt.Errorf("parsing credentials: %w", err)
	}
	return &restService{
		hc:           conf.Client(context.Background()),
		baseURL:      sheetsBaseURL,
		driveURL:     driveFilesURL,
		rateInterval: sheetsRateInterval,
		rateBurst:    sheetsRateBurst,
	}, nil
}

func (s *restService) waitForSlot() {
	if s.rateInterval <= 0 {
		return
	}
	s.rateMu.Lock()
	now := time.Now()
	if s.rateLast.IsZero() {
		s.rateTokens = float64(s.rateBurst)
	} else {
		s.rateTokens += now.Sub(s.rateLast).Seconds() / s.rateInterval.Seconds()
		if s.rateTokens > float64(s.rateBurst) {
			s.rateTokens = float64(s.rateBurst)
		}
	}
	s.rateLast = now
	s.rateTokens--
	var wait time.Duration
	if s.rateTokens < 0 {
		wait = time.Duration(-s.rateTokens * float64(s.rateInterval))
	}
	s.rateMu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
}

type valueRange struct {
	Range  string          `json:"range,omitempty"`
	Values [][]interface{} `json:"values,omitempty"`
}

func (s *restService) doJSON(method, reqURL string, body, out interface{}) (err error) {
	s.waitForSlot()
	defer func() {
		s.healthMu.Lock()
		if err != nil {
			if s.lastErr == nil {
				s.downSince = time.Now()
			}
			s.lastErr = err
		} else {
			s.lastErr = nil
			s.downSince = time.Time{}
		}
		s.healthMu.Unlock()
	}()

	var reqBody io.Reader
	if body != nil {
		data, merr := json.Marshal(body)
		if merr != nil {
			return fmt.Errorf("encoding request: %w", merr)
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
		decoder := json.NewDecoder(resp.Body)
		decoder.UseNumber()
		if err := decoder.Decode(out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

func (s *restService) health() (down bool, since time.Time, err error) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	return s.lastErr != nil, s.downSince, s.lastErr
}

func (s *restService) valuesURL(spreadsheetID, rng, suffix, query string) string {
	return s.baseURL + spreadsheetID + "/values/" + url.PathEscape(rng) + suffix + "?" + query
}

func (s *restService) valuesGet(spreadsheetID, rng string) (valueRange, error) {
	var out valueRange
	err := s.doJSON(http.MethodGet, s.valuesURL(spreadsheetID, rng, "", "valueRenderOption=UNFORMATTED_VALUE"), nil, &out)
	return out, err
}

func (s *restService) valuesUpdate(spreadsheetID, rng string, values [][]interface{}) error {
	return s.doJSON(http.MethodPut, s.valuesURL(spreadsheetID, rng, "", "valueInputOption=RAW"),
		valueRange{Values: dollarCells(values)}, nil)
}

func (s *restService) valuesAppend(spreadsheetID, rng string, values [][]interface{}) error {
	return s.doJSON(http.MethodPost, s.valuesURL(spreadsheetID, rng, ":append", "valueInputOption=RAW"),
		valueRange{Values: dollarCells(values)}, nil)
}

func (s *restService) valuesBatchUpdate(spreadsheetID string, data []valueRange) error {
	for i := range data {
		data[i].Values = dollarCells(data[i].Values)
	}
	body := struct {
		ValueInputOption string       `json:"valueInputOption"`
		Data             []valueRange `json:"data"`
	}{"RAW", data}
	return s.doJSON(http.MethodPost, s.baseURL+spreadsheetID+"/values:batchUpdate", body, nil)
}

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

func dollarCells(rows [][]interface{}) [][]interface{} {
	result := make([][]interface{}, len(rows))
	for i, row := range rows {
		result[i] = append([]interface{}(nil), row...)
		for j, cell := range row {
			if amountCents, ok := cell.(money.Cents); ok {
				result[i][j] = amountCents.Dollars()
			}
		}
	}
	return result
}

func (s *restService) clearHistoryValues(spreadsheetID string, columnsByTitle map[string]int, cashMetadataEnds map[int]int) error {
	var out struct {
		Sheets []struct {
			Properties struct {
				ID    int    `json:"sheetId"`
				Title string `json:"title"`
			} `json:"properties"`
		} `json:"sheets"`
	}
	if err := s.doJSON(http.MethodGet, s.baseURL+spreadsheetID+"?fields=sheets.properties(sheetId,title)", nil, &out); err != nil {
		return fmt.Errorf("looking up history tab IDs: %w", err)
	}
	type gridRange struct {
		SheetID          int `json:"sheetId"`
		StartRowIndex    int `json:"startRowIndex"`
		EndColumnIndex   int `json:"endColumnIndex"`
		EndRowIndex      int `json:"endRowIndex,omitempty"`
		StartColumnIndex int `json:"startColumnIndex,omitempty"`
	}
	type updateCells struct {
		Range  gridRange `json:"range"`
		Fields string    `json:"fields"`
	}
	type request struct {
		UpdateCells updateCells `json:"updateCells"`
	}
	body := struct {
		Requests []request `json:"requests"`
	}{}
	found := 0
	for _, sheet := range out.Sheets {
		if columns, exists := columnsByTitle[sheet.Properties.Title]; exists {
			found++
			body.Requests = append(body.Requests, request{UpdateCells: updateCells{
				Range:  gridRange{SheetID: sheet.Properties.ID, StartRowIndex: 1, EndColumnIndex: columns},
				Fields: "userEnteredValue",
			}})
			if sheet.Properties.Title == cashHistoryTab {
				for row, end := range cashMetadataEnds {
					body.Requests = append(body.Requests, request{UpdateCells: updateCells{
						Range:  gridRange{SheetID: sheet.Properties.ID, StartRowIndex: row, EndRowIndex: row + 1, StartColumnIndex: columns, EndColumnIndex: end},
						Fields: "userEnteredValue",
					}})
				}
			}
		}
	}
	if found != len(columnsByTitle) {
		return fmt.Errorf("one or more tracker history tabs are missing")
	}
	return s.doJSON(http.MethodPost, s.baseURL+spreadsheetID+":batchUpdate", body, nil)
}

type gridWrite struct {
	tab         string
	row, column int
	values      [][]interface{}
}

func (s *restService) writeHistoryGrid(spreadsheetID string, writes []gridWrite) error {
	type gridSize struct {
		Rows    int `json:"rowCount"`
		Columns int `json:"columnCount"`
	}
	type tab struct {
		ID    int      `json:"sheetId"`
		Title string   `json:"title"`
		Size  gridSize `json:"gridProperties"`
	}
	var out struct {
		Sheets []struct {
			Properties tab `json:"properties"`
		} `json:"sheets"`
	}
	if err := s.doJSON(http.MethodGet, s.baseURL+spreadsheetID+"?fields=sheets.properties(sheetId,title,gridProperties(rowCount,columnCount))", nil, &out); err != nil {
		return fmt.Errorf("reading history grid sizes: %w", err)
	}
	tabs := make(map[string]tab)
	for _, sheet := range out.Sheets {
		tabs[sheet.Properties.Title] = sheet.Properties
	}

	var requests []map[string]interface{}
	for _, write := range writes {
		props, exists := tabs[write.tab]
		if !exists {
			return fmt.Errorf("history tab %q is missing", write.tab)
		}
		columns := write.column
		rows := make([]map[string]interface{}, len(write.values))
		for i, row := range dollarCells(write.values) {
			columns = max(columns, write.column+len(row))
			cells := make([]map[string]interface{}, len(row))
			for j, value := range row {
				name := "numberValue"
				if _, ok := value.(string); ok {
					name = "stringValue"
				}
				cells[j] = map[string]interface{}{"userEnteredValue": map[string]interface{}{name: value}}
			}
			rows[i] = map[string]interface{}{"values": cells}
		}
		for _, size := range []struct {
			dimension string
			length    int
		}{
			{"ROWS", write.row + len(rows) - props.Size.Rows},
			{"COLUMNS", columns - props.Size.Columns},
		} {
			if size.length > 0 {
				requests = append(requests, map[string]interface{}{"appendDimension": map[string]interface{}{
					"sheetId": props.ID, "dimension": size.dimension, "length": size.length,
				}})
			}
		}
		props.Size.Rows = max(props.Size.Rows, write.row+len(rows))
		props.Size.Columns = max(props.Size.Columns, columns)
		tabs[write.tab] = props
		requests = append(requests, map[string]interface{}{"updateCells": map[string]interface{}{
			"start": map[string]int{"sheetId": props.ID, "rowIndex": write.row, "columnIndex": write.column},
			"rows":  rows, "fields": "userEnteredValue",
		}})
	}
	return s.doJSON(http.MethodPost, s.baseURL+spreadsheetID+":batchUpdate", map[string]interface{}{"requests": requests}, nil)
}
