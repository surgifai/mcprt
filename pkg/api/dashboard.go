package api

import (
	_ "embed"
	"html/template"
	"net/http"
)

//go:embed dashboard.html
var dashboardHTML string

var dashboardTmpl = template.Must(template.New("dashboard").Parse(dashboardHTML))

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Inject the API key so the JS can authenticate its fetch calls.
	// Served over localhost only (SSH tunnel), so embedding the key in HTML is acceptable.
	_ = dashboardTmpl.Execute(w, map[string]string{"APIKey": h.apiKey})
}
