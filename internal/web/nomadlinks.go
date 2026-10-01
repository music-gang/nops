package web

import "github.com/music-gang/nops/internal/nomadx"

// nomadJobURL is the page of a job in the Nomad UI, or "" when the dashboard
// does not know where a person opens Nomad (-nomad-ui-url unset).
func (s *server) nomadJobURL(namespace, jobID string) string {
	return nomadx.UIJobURL(s.nomadUI, namespace, jobID)
}

// nomadDeploymentsURL is the Deployments tab of a job in the Nomad UI: where its
// deployments, their canaries and the button to promote them are. Nomad's UI
// has no page of its own for one deployment. "" when there is no link.
func (s *server) nomadDeploymentsURL(namespace, jobID string) string {
	if u := s.nomadJobURL(namespace, jobID); u != "" {
		return u + "/deployments"
	}
	return ""
}
