package web

import "net/url"

// nomadJobURL is the page of a job in the Nomad UI, or "" when the dashboard
// does not know where a person opens Nomad (-nomad-ui-url unset): no link,
// never one made from -nomad-addr, which is the address nops uses and often not
// one a browser can reach. The UI names a job by its ID and namespace in one
// path segment, so an ID with a "/" (a hook's dispatched child) is escaped.
func (s *server) nomadJobURL(namespace, jobID string) string {
	if s.nomadUI == "" {
		return ""
	}
	return s.nomadUI + "/ui/jobs/" + url.PathEscape(jobID) + "@" + url.PathEscape(namespace)
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
