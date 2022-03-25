package authorization

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/kuadrant/authorino/pkg/common"
	"github.com/kuadrant/authorino/pkg/common/auth_credentials"
	"github.com/kuadrant/authorino/pkg/cron"
	"github.com/kuadrant/authorino/pkg/log"

	opaParser "github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/rego"
)

const (
	policyTemplate = `package %s
default allow = false
%s`
	policyUIDHashSeparator = "|"
	allowQuery             = "allow"

	msg_opaPolicyInvalidResponseError        = "invalid response from policy evaluation"
	msg_OpaPolicyPrecompileError             = "failed to precompile policy"
	msg_opaPolicyDownloadError               = "failed to download policy from external registry"
	msg_opaPolicyRefreshFromRegistryError    = "failed to refresh policy from external registry"
	msg_opaPolicyRefreshFromRegistrySkipped  = "external policy unchanged"
	msg_opaPolicyRefreshFromRegistrySuccess  = "policy updated from external registry"
	msg_opaPolicyRefreshFromRegistryDisabled = "auto-refresh of external policy disabled"
)

func NewOPAAuthorization(policyName string, rego string, externalSource *OPAExternalSource, allValues bool, nonce int, ctx context.Context) (*OPA, error) {
	logger := log.FromContext(ctx).WithName("opa")

	pullFromRegistry := rego == "" && externalSource != nil && externalSource.Endpoint != ""

	if pullFromRegistry {
		if downloadedRego, err := externalSource.downloadRegoDataFromUrl(); err != nil {
			logger.Error(err, msg_opaPolicyDownloadError, "policy", policyName, "endpoint", externalSource.Endpoint)
			return nil, err
		} else {
			rego = downloadedRego
		}
	}

	o := &OPA{
		ExternalSource: externalSource,
		AllValues:      allValues,
		policyName:     policyName,
		policyUID:      generatePolicyUID(policyName, rego, nonce),
		opaContext:     context.TODO(),
	}

	if _, err := o.updateRego(rego, ctx, true); err != nil {
		return nil, err
	} else {
		if pullFromRegistry {
			externalSource.setupRefresher(log.IntoContext(ctx, logger), o)
		}
		return o, nil
	}
}

type OPA struct {
	Rego           string `yaml:"rego"`
	ExternalSource *OPAExternalSource
	AllValues      bool

	opaContext context.Context
	policy     *rego.PreparedEvalQuery
	policyName string
	policyUID  string

	mu sync.Mutex
}

func (opa *OPA) Call(pipeline common.AuthPipeline, ctx context.Context) (interface{}, error) {
	var authJSON interface{}
	if err := json.Unmarshal([]byte(pipeline.GetAuthorizationJSON()), &authJSON); err != nil {
		return false, err
	} else {
		options := rego.EvalInput(authJSON)
		results, err := opa.policy.Eval(opa.opaContext, options)

		if err != nil {
			return nil, err
		} else if len(results) == 0 {
			return nil, fmt.Errorf(msg_opaPolicyInvalidResponseError)
		} else if allowed, ok := results[0].Bindings[allowQuery].(bool); !ok || !allowed {
			return nil, fmt.Errorf(unauthorizedErrorMsg)
		} else {
			return results[0].Bindings, nil
		}
	}
}

// Clean ensures the goroutine started by ExternalSource.setupRefresher is cleaned up
func (opa *OPA) Clean(_ context.Context) error {
	if opa.ExternalSource == nil {
		return nil
	}

	return opa.ExternalSource.cleanupRefresher()
}

func (opa *OPA) precompilePolicy() error {
	policyName := fmt.Sprintf(`authorino.authz["%s"]`, opa.policyUID)
	policyContent := fmt.Sprintf(policyTemplate, policyName, opa.Rego)
	policyFileName := opa.policyUID + ".rego"
	queryTemplate := `%s = object.get(data.` + policyName + `, "%s", null)`

	var module *opaParser.Module
	queries := []string{fmt.Sprintf(queryTemplate, allowQuery, allowQuery)}
	var err error

	if module, err = opaParser.ParseModule(policyFileName, policyContent); err != nil {
		return err
	}

	if opa.AllValues {
		rules := map[string]interface{}{allowQuery: nil}
		for _, rule := range module.Rules {
			name := string(rule.Head.Name)
			if _, found := rules[name]; !found {
				queries = append(queries, fmt.Sprintf(queryTemplate, name, name))
				rules[name] = nil
			}
		}
	}

	r := rego.New(
		rego.Query(strings.Join(queries, ";")),
		rego.ParsedModule(module),
	)

	if regoPolicy, err := r.PrepareForEval(opa.opaContext); err != nil {
		return err
	} else {
		opa.policy = &regoPolicy
		return nil
	}
}

func (opa *OPA) updateRego(rego string, ctx context.Context, force bool) (bool, error) {
	opa.mu.Lock()
	defer opa.mu.Unlock()

	newRego := cleanUpRegoDocument(rego)
	currentRego := opa.Rego

	if !force && hash(newRego) == hash(currentRego) {
		return false, nil
	}

	opa.Rego = newRego

	if err := opa.precompilePolicy(); err != nil {
		opa.Rego = currentRego
		log.FromContext(ctx).Error(err, msg_OpaPolicyPrecompileError, "policy", opa.policyName)
		return false, err
	}

	return true, nil
}

func cleanUpRegoDocument(rego string) string {
	r, _ := regexp.Compile("(\\s)*package.*[;\\n]+")
	return r.ReplaceAllString(rego, "")
}

func generatePolicyUID(policyName string, policyContent string, nonce int) string {
	return hash(fmt.Sprint(nonce) + policyUIDHashSeparator + policyName + policyUIDHashSeparator + policyContent)
}

func hash(s string) string {
	data := []byte(s)
	return fmt.Sprintf("%x", md5.Sum(data))
}

type responseOpaJson struct {
	Result resultJson `json:"result"`
}

type resultJson struct {
	Raw string `json:"raw"`
}

type OPAExternalSource struct {
	Endpoint     string
	SharedSecret string
	auth_credentials.AuthCredentials
	TTL       int
	refresher cron.Worker
}

func (ext *OPAExternalSource) downloadRegoDataFromUrl() (string, error) {
	req, err := ext.BuildRequestWithCredentials(context.TODO(), ext.Endpoint, "GET", ext.SharedSecret, nil)
	if err != nil {
		return "", err
	}

	if resp, err := http.DefaultClient.Do(req); err != nil {
		return "", fmt.Errorf("failed to fetch Rego config: %v", err)
	} else {
		defer resp.Body.Close()

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("unable to read response body: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("%s: %s", resp.Status, body)
		}

		result := string(body)
		//json
		if resp.Header["Content-Type"][0] == "application/json" {
			var jsonResponse responseOpaJson
			if err := json.Unmarshal(body, &jsonResponse); err != nil {
				return "", fmt.Errorf("unable to unmarshal json response: %v", err)
			}
			result = jsonResponse.Result.Raw
		}
		return result, nil
	}
}

func (ext *OPAExternalSource) setupRefresher(ctx context.Context, opa *OPA) {
	logger := log.FromContext(ctx).WithValues("policy", opa.policyName, "endpoint", ext.Endpoint)

	var startErr error

	ext.refresher, startErr = cron.StartWorker(ctx, ext.TTL, func() {
		if downloadedRego, err := ext.downloadRegoDataFromUrl(); err == nil {
			if updated, err := opa.updateRego(downloadedRego, ctx, false); updated {
				logger.Info(msg_opaPolicyRefreshFromRegistrySuccess)
			} else {
				if err != nil {
					logger.Error(err, msg_opaPolicyRefreshFromRegistryError)
				} else {
					logger.V(1).Info(msg_opaPolicyRefreshFromRegistrySkipped)
				}
			}
		} else {
			logger.Error(err, msg_opaPolicyDownloadError)
		}
	})

	if startErr != nil {
		logger.V(1).Info(msg_opaPolicyRefreshFromRegistryDisabled, "reason", startErr)
	}
}

func (ext *OPAExternalSource) cleanupRefresher() error {
	if ext.refresher == nil {
		return nil
	}
	return ext.refresher.Stop()
}
