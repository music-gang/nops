package nomadx

import "net/url"

// UIJobURL is the page of a job in the Nomad UI at ui (without a trailing
// slash), or "" when ui is empty: no link, never one made from the address
// nops uses, which is often not one a browser can reach. The UI names a job by
// its ID and namespace in one path segment, so an ID with a "/" (a hook's
// dispatched child) is escaped.
func UIJobURL(ui, namespace, jobID string) string {
	if ui == "" {
		return ""
	}
	return ui + "/ui/jobs/" + url.PathEscape(jobID) + "@" + url.PathEscape(namespace)
}
