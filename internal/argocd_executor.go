package argocd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/argoproj/argo-cd/v2/pkg/apiclient/application"
	"github.com/argoproj/argo-cd/v2/pkg/apiclient/settings"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	argodiff "github.com/argoproj/argo-cd/v2/util/argo/diff"
	"github.com/argoproj/argo-cd/v2/util/io"
	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v3/pkg/plugins/executor"
	"github.com/argoproj/gitops-engine/pkg/sync/hook"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/pointer"

	"github.com/argoproj/argo-cd/v2/pkg/apiclient"
)

type ApiExecutor struct {
	apiClient  apiclient.Client
	agentToken string
}

func NewApiExecutor(apiClient apiclient.Client, agentToken string) ApiExecutor {
	return ApiExecutor{apiClient: apiClient, agentToken: agentToken}
}

func (e *ApiExecutor) Authorize(req *http.Request) error {
	auth := req.Header.Get("Authorization")
	if auth != "Bearer "+e.agentToken {
		return fmt.Errorf("invalid agent token")
	}
	return nil
}

func (e *ApiExecutor) Execute(args executor.ExecuteTemplateArgs) executor.ExecuteTemplateReply {
	pluginJSON, err := args.Template.Plugin.MarshalJSON()
	if err != nil {
		err = fmt.Errorf("failed to marshal plugin to JSON from workflow spec: %w", err)
		log.Println(err.Error())
		return errorResponse(err)
	}

	plugin := &PluginSpec{}
	err = json.Unmarshal(pluginJSON, plugin)
	if err != nil {
		err = fmt.Errorf("failed to unmarshal plugin JSON to plugin struct: %w", err)
		log.Println(err.Error())
		return errorResponse(err)
	}

	if plugin.ArgoCD == nil {
		log.Println("unsupported plugin type")
		return executor.ExecuteTemplateReply{} // unsupported plugin
	}

	output, err := e.runAction(*plugin.ArgoCD)
	if err != nil {
		return failedResponse(wfv1.Progress(fmt.Sprintf("0/1")), fmt.Errorf("action failed: %w", err))
	}

	return executor.ExecuteTemplateReply{
		Node: &wfv1.NodeResult{
			Phase:    wfv1.NodeSucceeded,
			Message:  "Action completed",
			Progress: "1/1",
			Outputs: &wfv1.Outputs{
				Result: pointer.String(output),
			},
		},
	}
}

// runAction runs the given action and returns outputs or errors, if any.
func (e *ApiExecutor) runAction(action ActionSpec) (out string, err error) {
	closer, appClient, err := e.apiClient.NewApplicationClient()
	if err != nil {
		return "", fmt.Errorf("failed to initialize Application API client: %w", err)
	}
	defer io.Close(closer)

	closer, settingsClient, err := e.apiClient.NewSettingsClient()
	if err != nil {
		return "", fmt.Errorf("failed to initialize Application API client: %w", err)
	}
	defer io.Close(closer)

	if action.App == nil {
		return "", errors.New("action is missing a valid action type (i.e. an 'app' block)")
	}
	if action.App.Sync != nil && action.App.Diff != nil {
		return "", errors.New("action has multiple types of action defined (both sync and diff)")
	}
	if action.App.Sync == nil && action.App.Diff == nil {
		return "", errors.New("app action has no action type specified (must be sync or diff)")
	}

	if action.App.Sync != nil {
		err = syncAppsParallel(*action.App.Sync, action.Timeout, appClient)
		if err != nil {
			return "", fmt.Errorf("failed to sync apps: %w", err)
		}
	}
	if action.App.Diff != nil {
		out, err = diffApp(*action.App.Diff, action.Timeout, appClient, settingsClient)
		if err != nil {
			return "", fmt.Errorf("failed to diff app: %w", err)
		}
	}
	return out, err
}

// syncAppsParallel loops over the apps in a SyncAction and syncs them in parallel. It waits for all responses and then
// aggregates any errors.
func syncAppsParallel(action SyncAction, timeout string, appClient application.ApplicationServiceClient) error {
	var apps []App
	err := yaml.Unmarshal([]byte(action.Apps), &apps)
	if err != nil {
		return fmt.Errorf("failed to unmarshal apps: %w", err)
	}
	var options []string
	err = yaml.Unmarshal([]byte(action.Options), &options)
	if err != nil {
		return fmt.Errorf("failed to unmarshal options: %w", err)
	}
	ctx, cancel, err := durationStringToContext(timeout)
	if err != nil {
		return fmt.Errorf("failed get action context: %w", err)
	}
	defer cancel()
	wg := sync.WaitGroup{}
	errChan := make(chan error, len(action.Apps))
	for _, app := range apps {
		app := app
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := appClient.Sync(ctx, &application.ApplicationSyncRequest{
				Name:         pointer.String(app.Name),
				AppNamespace: pointer.String(app.Namespace),
				SyncOptions:  &application.SyncOptions{Items: options},
			})
			if err != nil {
				errChan <- fmt.Errorf("failed to sync app %q: %w", app.Name, err)
			}
		}()
	}
	go func() {
		wg.Wait()
		close(errChan)
	}()
	var syncErrors []string
	for err := range errChan {
		syncErrors = append(syncErrors, err.Error())
	}
	if len(syncErrors) > 0 {
		return errors.New(strings.Join(syncErrors, ", "))
	}
	return nil
}

func diffApp(action DiffAction, timeout string, appClient application.ApplicationServiceClient, settingsClient settings.SettingsServiceClient) (string, error) {
	ctx, cancel, err := durationStringToContext(timeout)
	if err != nil {
		return "", fmt.Errorf("failed get action context: %w", err)
	}
	defer cancel()
	app, err := appClient.Get(context.Background(), &application.ApplicationQuery{Name: &action.App.Name, Refresh: getRefreshType(action.Refresh, action.HardRefresh)})
	if err != nil {
		return "", fmt.Errorf("failed to get application: %w", err)
	}
	resources, err := appClient.ManagedResources(context.Background(), &application.ResourcesQuery{ApplicationName: &action.App.Name})
	if err != nil {
		return "", fmt.Errorf("failed to get managed resources for app: %w", err)
	}
	liveObjs, err := liveObjects(resources.Items)
	if err != nil {
		return "", fmt.Errorf("failed to get live objects: %w", err)
	}

	res, err := appClient.GetManifests(ctx, &application.ApplicationManifestQuery{
		Name:         pointer.String(action.App.Name),
		AppNamespace: pointer.String(action.App.Namespace),
		Revision:     pointer.String(action.Revision),
	})
	if err != nil {
		return "", fmt.Errorf("failed to diff app: %w", err)
	}

	var unstructureds []*unstructured.Unstructured
	for _, manifest := range res.Manifests {
		obj, err := v1alpha1.UnmarshalToUnstructured(manifest)
		if err != nil {
			return "", fmt.Errorf("failed to unmarshal manifest to unstructured: %w", err)
		}
		unstructureds = append(unstructureds, obj)
	}
	groupedObjs, err := groupObjsByKey(unstructureds, liveObjs, app.Spec.Destination.Namespace)
	if err != nil {
		return "", fmt.Errorf("failed to group objects by key: %w", err)
	}

	argoSettings, err := settingsClient.Get(context.Background(), &settings.SettingsQuery{})
	if err != nil {
		return "", fmt.Errorf("failed to get argo settings: %w", err)
	}

	items, err := groupObjsForDiff(resources, groupedObjs, []objKeyLiveTarget{}, argoSettings, action.App.Name)
	if err != nil {
		return "", fmt.Errorf("failed to group objects for diff: %w", err)
	}

	diff := ""
	for _, item := range items {
		if item.target != nil && hook.IsHook(item.target) || item.live != nil && hook.IsHook(item.live) {
			continue
		}
		overrides := make(map[string]v1alpha1.ResourceOverride)
		for k := range argoSettings.ResourceOverrides {
			val := argoSettings.ResourceOverrides[k]
			overrides[k] = *val
		}

		// TODO remove hardcoded IgnoreAggregatedRoles and retrieve the
		// compareOptions in the protobuf
		ignoreAggregatedRoles := false
		diffConfig, err := argodiff.NewDiffConfigBuilder().
			WithDiffSettings(app.Spec.IgnoreDifferences, overrides, ignoreAggregatedRoles).
			WithTracking(argoSettings.AppLabelKey, argoSettings.TrackingMethod).
			WithNoCache().
			Build()
		if err != nil {
			return "", fmt.Errorf("failed to build diff config: %w", err)
		}

		diffRes, err := argodiff.StateDiff(item.live, item.target, diffConfig)
		if err != nil {
			return "", fmt.Errorf("failed to build state diff: %w", err)
		}

		if diffRes.Modified || item.target == nil || item.live == nil {
			fmt.Println("diffRes.Modified", diffRes.Modified)

			var live *unstructured.Unstructured
			var target *unstructured.Unstructured
			if item.target != nil && item.live != nil {
				target = &unstructured.Unstructured{}
				live = item.live
				err = json.Unmarshal(diffRes.PredictedLive, target)
				if err != nil {
					return "", fmt.Errorf("failed to unmarshal predicted live: %w", err)
				}
			} else {
				live = item.live
				target = item.target
			}

			newDiff, err := GetDiff(live, target)
			if err != nil {
				return "", fmt.Errorf("failed to get diff: %w", err)
			}
			diff += newDiff
		}
	}

	return diff, nil
}

// durationStringToContext parses a duration string and returns a context and cancel function. If timeout is empty, the
// context is context.Background().
func durationStringToContext(timeout string) (ctx context.Context, cancel func(), err error) {
	ctx = context.Background()
	cancel = func() {}
	if timeout != "" {
		duration, err := time.ParseDuration(timeout)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse timeout: %w", err)
		}
		ctx, cancel = context.WithTimeout(ctx, duration)
	}
	return ctx, cancel, nil
}

func errorResponse(err error) executor.ExecuteTemplateReply {
	return executor.ExecuteTemplateReply{
		Node: &wfv1.NodeResult{
			Phase:    wfv1.NodeError,
			Message:  err.Error(),
			Progress: wfv1.ProgressZero,
		},
	}
}

func failedResponse(progress wfv1.Progress, err error) executor.ExecuteTemplateReply {
	return executor.ExecuteTemplateReply{
		Node: &wfv1.NodeResult{
			Phase:    wfv1.NodeFailed,
			Message:  err.Error(),
			Progress: progress,
		},
	}
}
