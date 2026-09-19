package dashboard

// SWT-67 B21: the full-screen shell. A full-page reload exits the browser's
// fullscreen, and the board's auto-refresh IS a full-page reload, so FULL on
// /tasks itself would last until the next refresh. GET /kiosk holds the board
// in an iframe instead: the shell's document goes fullscreen once, on a tap,
// and the board reloads inside it. It reads nothing and executes nothing.

import (
	"net/http"
	"net/url"
)

type kioskData struct {
	// BoardURL is the framed board: always refreshing, carrying only the
	// project filter.
	BoardURL string
}

// kioskBoardURL is the board the shell frames.
func kioskBoardURL(project string) string {
	v := url.Values{"refresh": {"on"}}
	if project != "" {
		v.Set("project", project)
	}
	return "/tasks?" + v.Encode()
}

// kioskURL is the shell for a project filter ("" = all projects).
func kioskURL(project string) string {
	if project == "" {
		return "/kiosk"
	}
	return "/kiosk?" + url.Values{"project": {project}}.Encode()
}

func (s *Server) showKiosk(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	data := kioskData{BoardURL: kioskBoardURL(project)}
	if err := s.tmpl.ExecuteTemplate(w, "kiosk.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
