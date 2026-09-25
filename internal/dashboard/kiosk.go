package dashboard

// SWT-67 B21: the full-screen shell. A full-page navigation exits the browser's
// fullscreen. Since SWT-89 the board updates in place, but it still navigates
// (a row tap, a verb's redirect, a version reload after a deploy, a login), so
// FULL on /tasks itself would end at the first of those. GET /kiosk holds the
// board in an iframe instead: the shell's document goes fullscreen once, on a
// tap, and the board navigates inside it. It reads nothing and executes nothing.

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
