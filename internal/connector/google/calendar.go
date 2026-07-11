package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// CalendarClient reads the primary calendar. There is no per-user path
// segment — identity rides on hc (the per-account oauth2 client in
// production, an X-Fake-User transport in tests).
type CalendarClient struct {
	hc      *http.Client
	baseURL string
}

func NewCalendarClient(hc *http.Client, baseURL string) *CalendarClient {
	if baseURL == "" {
		baseURL = "https://www.googleapis.com"
	}
	return &CalendarClient{hc: hc, baseURL: strings.TrimRight(baseURL, "/")}
}

type calListPage struct {
	Items         []json.RawMessage `json:"items"`
	NextPageToken string            `json:"nextPageToken"`
	NextSyncToken string            `json:"nextSyncToken"`
}

// errSyncTokenGone signals HTTP 410: the sync token expired; re-window.
var errSyncTokenGone = errors.New("calendar sync token gone (HTTP 410)")

// ListEvents returns one page of primary-calendar events. Exactly one of
// syncToken or (timeMin, timeMax) drives the query.
func (c *CalendarClient) ListEvents(ctx context.Context, syncToken, timeMin, timeMax, pageToken string) (calListPage, error) {
	query := url.Values{"singleEvents": {"true"}}
	if syncToken != "" {
		query.Set("syncToken", syncToken)
	} else {
		query.Set("timeMin", timeMin)
		query.Set("timeMax", timeMax)
	}
	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}

	u := c.baseURL + "/calendar/v3/calendars/primary/events?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return calListPage{}, fmt.Errorf("build events request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return calListPage{}, fmt.Errorf("GET events: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return calListPage{}, fmt.Errorf("read events: %w", err)
	}
	if resp.StatusCode == http.StatusGone {
		return calListPage{}, errSyncTokenGone
	}
	if resp.StatusCode != http.StatusOK {
		return calListPage{}, &apiError{Status: resp.StatusCode, Body: string(body), Path: "/calendar/v3/calendars/primary/events"}
	}
	var page calListPage
	if err := json.Unmarshal(body, &page); err != nil {
		return calListPage{}, fmt.Errorf("parse events page: %w", err)
	}
	return page, nil
}
