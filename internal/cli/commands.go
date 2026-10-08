package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/music-gang/nops/internal/secret"
)

// commands is every command of the client, in the order the usage lists them.
// Each one is a call of the API of docs/api.md.
var commands = []command{
	{name: "jobs", help: "list the jobs with their sync state", run: runJobs},
	{name: "job", arg: "<job>", help: "show a job with its drift and deployments", run: runJob},
	{name: "deployments", help: "list the active and the latest finished deployments", run: runDeployments},
	{name: "deployment", arg: "<id>", help: "show a deployment with its diff, hook runs and events", run: runDeployment},
	{name: "approve", arg: "<id>", yes: true, help: "show the diff of a deployment and approve it", run: runApprove},
	{name: "reject", arg: "<id>", help: "reject a deployment", run: action(http.MethodPost, deploymentAction("reject"), "rejected")},
	{name: "promote", arg: "<id>", help: "promote the canaries of a deployment", run: action(http.MethodPost, deploymentAction("promote"), "promoted")},
	{name: "retry", arg: "<id>", help: "retry a failed or rejected deployment", run: runRetry},
	{name: "pause", arg: "<job>", why: true, help: "pause a job", run: runPause},
	{name: "resume", arg: "<job>", help: "resume a paused job", run: action(http.MethodPost, jobAction("resume"), "resumed")},
	{name: "deploy-now", arg: "<job>", yes: true, help: "deploy a held job outside its sync window", run: runDeployNow},
	{name: "fetch", help: "ask for a git poll now", run: action(http.MethodPost, func(*call) string { return "/api/fetch" }, "asked for a poll")},
	{name: "acl policy list", help: "list the ACL policies", run: runPolicyList},
	{name: "acl policy info", arg: "<name>", help: "show an ACL policy with its rules", run: runPolicyInfo},
	{name: "acl policy apply", arg: "<name> <file>", flags: policyFlags, help: "create or replace an ACL policy from a file of rules", run: runPolicyApply},
	{name: "acl policy delete", arg: "<name>", help: "delete an ACL policy", run: action(http.MethodDelete, policyPath, "deleted")},
	{name: "acl binding-rule list", help: "list the binding rules", run: runBindingRuleList},
	{name: "acl binding-rule info", arg: "<id>", help: "show a binding rule", run: runBindingRuleInfo},
	{name: "acl binding-rule create", flags: bindingRuleFlags, help: "create a binding rule", run: runBindingRuleCreate},
	{name: "acl binding-rule update", arg: "<id>", flags: bindingRuleFlags, help: "replace a binding rule", run: runBindingRuleUpdate},
	{name: "acl binding-rule delete", arg: "<id>", help: "delete a binding rule", run: action(http.MethodDelete, bindingRulePath, "deleted")},
	{name: "acl token list", help: "list the tokens", run: runTokenList},
	{name: "acl token info", arg: "<accessor-id>", help: "show a token", run: runTokenInfo},
	{name: "acl token create", flags: tokenFlags, help: "create a token and print its secret once", run: runTokenCreate},
	{name: "acl token delete", arg: "<accessor-id>", help: "revoke a token", run: action(http.MethodDelete, tokenPath, "revoked")},
	{name: "acl token delete-sessions", arg: "<identity>", yes: true, help: "revoke every session of a person", run: runRevokeSessions},
	{name: "acl token delete-created", yes: true, flags: revokeCreatedFlags, help: "revoke every token a person or a token created, and the tokens those created", run: runRevokeCreated},
	{name: "acl token self", help: "show the token this client uses", run: runTokenSelf},
	{name: "secret generate", offline: true, help: "print a new random secret, offline", run: runSecretGenerate},
}

func runSecretGenerate(k *call) error {
	_, err := fmt.Fprintln(k.out, secret.New())
	return err
}

func deploymentAction(verb string) func(*call) string {
	return func(k *call) string { return deploymentPath(k.arg) + "/" + verb }
}

func jobAction(verb string) func(*call) string {
	return func(k *call) string { return jobPath(k.namespace, k.arg) + "/" + verb }
}

// action is a command that posts to an endpoint with no body and no answer: it
// says what it did, unless -json, where the API has nothing to print.
func action(method string, path func(*call) string, did string) func(*call) error {
	return func(k *call) error {
		if _, err := k.client.do(k.ctx, method, path(k), nil); err != nil {
			return err
		}
		if !k.asJSON {
			fmt.Fprintln(k.out, strings.TrimSpace(did+" "+k.arg))
		}
		return nil
	}
}

func runJobs(k *call) error {
	var r struct {
		Jobs []job `json:"jobs"`
	}
	raw, err := k.client.get(k.ctx, "/api/jobs", &r)
	if err != nil {
		return err
	}
	if k.asJSON {
		writeJSON(k.out, raw)
		return nil
	}
	writeJobs(k.out, r.Jobs)
	return nil
}

func runJob(k *call) error {
	var j jobDetail
	raw, err := k.client.get(k.ctx, jobPath(k.namespace, k.arg), &j)
	if err != nil {
		return err
	}
	if k.asJSON {
		writeJSON(k.out, raw)
		return nil
	}
	return writeJob(k.out, j)
}

func runDeployments(k *call) error {
	var r struct {
		Deployments []deployment `json:"deployments"`
	}
	raw, err := k.client.get(k.ctx, "/api/deployments", &r)
	if err != nil {
		return err
	}
	if k.asJSON {
		writeJSON(k.out, raw)
		return nil
	}
	writeDeployments(k.out, r.Deployments)
	return nil
}

func runDeployment(k *call) error {
	var d deploymentDetail
	raw, err := k.client.get(k.ctx, deploymentPath(k.arg), &d)
	if err != nil {
		return err
	}
	if k.asJSON {
		writeJSON(k.out, raw)
		return nil
	}
	return writeDeployment(k.out, d)
}

// runApprove shows what it approves, on stderr so a pipe keeps only the
// answer, and sends the spec hash it showed: a newer diff is refused by Nops.
func runApprove(k *call) error {
	var d deploymentDetail
	if _, err := k.client.get(k.ctx, deploymentPath(k.arg), &d); err != nil {
		return err
	}
	if err := writeDeployment(k.errOut, d); err != nil {
		return err
	}
	if err := k.confirm(fmt.Sprintf("\nApprove deployment %s of %s/%s?", d.ID, d.Namespace, d.Job)); err != nil {
		return err
	}
	body := map[string]string{"spec_hash": d.SpecHash}
	if _, err := k.client.do(k.ctx, http.MethodPost, deploymentPath(k.arg)+"/approve", body); err != nil {
		return err
	}
	if !k.asJSON {
		fmt.Fprintf(k.out, "approved %s\n", k.arg)
	}
	return nil
}

func runRetry(k *call) error {
	raw, err := k.client.do(k.ctx, http.MethodPost, deploymentPath(k.arg)+"/retry", nil)
	if err != nil {
		return err
	}
	return printNewDeployment(k, raw)
}

func runPause(k *call) error {
	var body any
	if k.reason != "" {
		body = map[string]string{"reason": k.reason}
	}
	if _, err := k.client.do(k.ctx, http.MethodPost, jobPath(k.namespace, k.arg)+"/pause", body); err != nil {
		return err
	}
	if !k.asJSON {
		fmt.Fprintf(k.out, "paused %s\n", k.arg)
	}
	return nil
}

// runDeployNow shows the drift the deployment would carry and sends the spec
// hash of the job as it read it, like approve.
func runDeployNow(k *call) error {
	var j jobDetail
	if _, err := k.client.get(k.ctx, jobPath(k.namespace, k.arg), &j); err != nil {
		return err
	}
	if j.SpecHash == "" {
		return fmt.Errorf("%s/%s has nothing to deploy now: it has no spec in git", k.namespace, k.arg)
	}
	if err := writeJob(k.errOut, j); err != nil {
		return err
	}
	if err := k.confirm(fmt.Sprintf("\nDeploy %s/%s now?", j.Namespace, j.Job)); err != nil {
		return err
	}
	raw, err := k.client.do(k.ctx, http.MethodPost, jobPath(k.namespace, k.arg)+"/deploy-now", map[string]string{"spec_hash": j.SpecHash})
	if err != nil {
		return err
	}
	return printNewDeployment(k, raw)
}

// printNewDeployment prints the ID the API answers for a retry or a Deploy
// now. Deploy now may answer none: the cycle was left to the detection loop.
func printNewDeployment(k *call, raw []byte) error {
	if k.asJSON {
		writeJSON(k.out, raw)
		return nil
	}
	var r struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("the answer is not what the API sends: %w", err)
	}
	if r.DeploymentID == "" {
		fmt.Fprintln(k.out, "requested: the deployment follows at the next detection cycle")
		return nil
	}
	fmt.Fprintf(k.out, "deployment %s\n", r.DeploymentID)
	return nil
}
