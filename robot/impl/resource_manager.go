package robotimpl

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jhump/protoreflect/desc"
	"go.uber.org/multierr"
	goutils "go.viam.com/utils"
	"go.viam.com/utils/pexec"
	"go.viam.com/utils/rpc"
	"golang.org/x/sync/errgroup"

	"go.viam.com/rdk/cloud"
	"go.viam.com/rdk/config"
	"go.viam.com/rdk/ftdc"
	"go.viam.com/rdk/grpc"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module/modmanager"
	modmanageroptions "go.viam.com/rdk/module/modmanager/options"
	modif "go.viam.com/rdk/module/modmaninterface"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/robot"
	"go.viam.com/rdk/robot/client"
	"go.viam.com/rdk/robot/web"
	"go.viam.com/rdk/services/shell"
	rutils "go.viam.com/rdk/utils"
	"go.viam.com/rdk/utils/contextutils"
)

func init() {
	if err := cleanAppImageEnv(); err != nil {
		//nolint
		fmt.Println("Error cleaning up app image environement:", err)
	}
}

var (
	resourceCloseTimeout    = 30 * time.Second
	errShellServiceDisabled = errors.New("shell service disabled in an untrusted environment")
)

// resourceManager manages the actual parts that make up a robot.
type resourceManager struct {
	resources *resource.Graph
	// modManagerLock controls access to the moduleManager and prevents a data race.
	// This may happen if Kill() or Close() is called concurrently with startModuleManager.
	modManagerLock sync.Mutex
	moduleManager  modif.ModuleManager
	opts           resourceManagerOptions
	logger         logging.Logger

	viz resource.Visualizer
}

type resourceManagerOptions struct {
	debug              bool
	fromCommand        bool
	allowInsecureCreds bool
	untrustedEnv       bool
	tlsConfig          *tls.Config
	ftdc               *ftdc.FTDC
}

// newResourceManager returns a properly initialized set of parts.
// moduleManager will not be initialized until startModuleManager is called.
func newResourceManager(
	opts resourceManagerOptions,
	logger logging.Logger,
) *resourceManager {
	resLogger := logger.Sublogger("resource_manager")
	var resourceGraph *resource.Graph
	if opts.ftdc != nil {
		resourceGraph = resource.NewGraphWithFTDC(logger, opts.ftdc)
	} else {
		resourceGraph = resource.NewGraph(logger)
	}

	return &resourceManager{
		resources: resourceGraph,
		opts:      opts,
		logger:    resLogger,
	}
}

func fromRemoteNameToRemoteNodeName(name string) resource.Name {
	return resource.NewName(client.RemoteAPI, name)
}

// ExportDot exports the resource graph as a DOT representation for visualization.
// DOT reference: https://graphviz.org/doc/info/lang.html
func (manager *resourceManager) ExportDot(index int) (resource.GetSnapshotInfo, error) {
	return manager.viz.GetSnapshot(index)
}

func (manager *resourceManager) startModuleManager(
	ctx context.Context,
	parentAddrs config.ParentSockAddrs,
	handleOrphanedResources func(context.Context, []resource.Name),
	untrustedEnv bool,
	viamHomeDir string,
	robotCloudID string,
	logger logging.Logger,
	packagesDir string,
	modPeerConnTracker *grpc.ModPeerConnTracker,
) error {
	mmOpts := modmanageroptions.Options{
		UntrustedEnv:            untrustedEnv,
		HandleOrphanedResources: handleOrphanedResources,
		ViamHomeDir:             viamHomeDir,
		RobotCloudID:            robotCloudID,
		PackagesDir:             packagesDir,
		FTDC:                    manager.opts.ftdc,
		ModPeerConnTracker:      modPeerConnTracker,
	}
	modmanager, err := modmanager.NewManager(ctx, parentAddrs, logger, mmOpts)
	if err != nil {
		return err
	}
	manager.modManagerLock.Lock()
	manager.moduleManager = modmanager
	manager.modManagerLock.Unlock()
	return nil
}

// addRemote adds a remote to the manager.
func (manager *resourceManager) addRemote(
	ctx context.Context,
	rr internalRemoteRobot,
	gNode *resource.GraphNode,
	c config.Remote,
) {
	rName := fromRemoteNameToRemoteNodeName(c.Name)
	if gNode == nil {
		gNode = resource.NewConfiguredGraphNode(resource.Config{
			ConvertedAttributes: &c,
		}, rr, builtinModel)
		if err := manager.resources.AddNode(rName, gNode); err != nil {
			manager.logger.CErrorw(ctx, "failed to add new node for remote", "name", rName, "error", err)
			return
		}
	} else {
		gNode.SwapResource(rr, builtinModel, manager.opts.ftdc)
	}
	manager.updateRemoteResourceNames(ctx, rName, rr, true)
}

func (manager *resourceManager) remoteResourceNames(remoteName resource.Name) []resource.Name {
	var filtered []resource.Name
	if _, ok := manager.resources.Node(remoteName); !ok {
		manager.logger.Errorw("trying to get remote resources of a non existing remote", "remote", remoteName)
	}
	children := manager.resources.GetAllChildrenOf(remoteName)
	for _, child := range children {
		if child.ContainsRemoteNames() {
			filtered = append(filtered, child)
		}
	}
	return filtered
}

var (
	unknownModel = resource.DefaultModelFamily.WithModel("unknown")
	builtinModel = resource.DefaultModelFamily.WithModel("builtin")
)

// maybe in the future this can become an actual resource with its own type
// so that one can depend on robots/remotes interchangeably.
type internalRemoteRobot interface {
	resource.Resource
	robot.Robot
}

// updateRemoteResourceNames is called when the Remote robot has changed (either connection or disconnection).
// It will pull the current remote resources and update the resource tree adding or removing nodes accordingly.
// The recreateAllClients flag will re-add all remote resource nodes if true and only new / uninitialized
// resource names if false. If any local resources are dependent on a remote resource two things can happen
//  1. The remote resource already is in the tree and nothing will happen.
//  2. A remote resource is being deleted but a local resource depends on it; it will be removed
//     and its local children will be destroyed.
func (manager *resourceManager) updateRemoteResourceNames(
	ctx context.Context,
	remoteName resource.Name,
	rr internalRemoteRobot,
	recreateAllClients bool,
) bool {
	logger := manager.logger.WithFields("remote", remoteName)
	logger.CDebugw(ctx, "updating remote resource names", "recreateAllClients", recreateAllClients)
	activeResourceNames := map[resource.Name]bool{}
	newResources := rr.ResourceNames()

	// The connection to the remote is broken. In this case, we mark each resource node
	// on this remote as disconnected but do not report any other changes.
	if newResources == nil {
		err := manager.resources.MarkReachability(remoteName, false)
		if err != nil {
			logger.Error(
				"unable to mark remote resources as unreachable",
				"error", err,
			)
		}
		return false
	}

	err := manager.resources.MarkReachability(remoteName, true)
	if err != nil {
		logger.Error(
			"unable to mark remote resources as reachable",
			"error", err,
		)
	}
	oldResources := manager.remoteResourceNames(remoteName)
	for _, res := range oldResources {
		activeResourceNames[res] = false
	}

	anythingChanged := false

	for _, resName := range newResources {
		remoteResName := resName
		resLogger := logger.WithFields("resource", remoteResName)
		res, err := rr.ResourceByName(remoteResName) // this returns a remote known OR foreign resource client
		if err != nil {
			if errors.Is(err, client.ErrMissingClientRegistration) {
				resLogger.CDebugw(ctx, "couldn't obtain remote resource interface",
					"reason", err)
			} else {
				resLogger.CErrorw(ctx, "couldn't obtain remote resource interface",
					"reason", err)
			}
			continue
		}
		resName = resName.PrependRemote(remoteName.Name)
		gNode, nodeAlreadyExists := manager.resources.Node(resName)
		if _, alreadyCurrent := activeResourceNames[resName]; alreadyCurrent {
			activeResourceNames[resName] = true
			if nodeAlreadyExists && !gNode.IsUninitialized() {
				// resources that enter this block represent those with names that already exist in the resource graph.
				// it is possible that we are switching to a new remote with a identical resource name(s), so we may
				// need to create these resource clients.
				if !recreateAllClients {
					// ticker event, likely no changes to remote resources, skip closing and re-adding duplicate name resource clients
					continue
				}
				// reconfiguration attempt, remote could have changed, so close all duplicate name remote resource clients and re-add new ones later
				resLogger.CDebugw(ctx, "attempting to remove remote resource")
				if err := manager.markChildrenForUpdate(resName); err != nil {
					resLogger.CErrorw(ctx,
						"failed to mark children of remote resource for update",
						"reason", err)
					continue
				}
				if err := gNode.Close(ctx); err != nil {
					resLogger.CErrorw(ctx,
						"failed to close remote resource node",
						"reason", err)
				}
			}
		}

		if nodeAlreadyExists {
			gNode.SwapResource(res, unknownModel, manager.opts.ftdc)
		} else {
			gNode = resource.NewConfiguredGraphNode(resource.Config{}, res, unknownModel)
			if err := manager.resources.AddNode(resName, gNode); err != nil {
				resLogger.CErrorw(ctx, "failed to add remote resource node", "error", err)
			}
		}

		err = manager.resources.AddChild(resName, remoteName)
		if err != nil {
			resLogger.CErrorw(ctx,
				"error while trying add node as a dependency of remote")
		} else {
			anythingChanged = true
		}
	}

	if anythingChanged {
		logger.CDebugw(ctx, "remote resource names update completed with changes to resource graph")
	} else {
		logger.CDebugw(ctx, "remote resource names update completed with no changes to resource graph")
	}

	for resName, isActive := range activeResourceNames {
		if isActive {
			continue
		}
		resLogger := logger.WithFields("resource", resName)
		resLogger.CDebugw(ctx, "attempting to remove remote resource")
		gNode, ok := manager.resources.Node(resName)
		if !ok || gNode.IsUninitialized() {
			resLogger.CDebugw(ctx, "remote resource already removed")
			continue
		}
		if err := manager.markChildrenForUpdate(resName); err != nil {
			resLogger.CErrorw(ctx,
				"failed to mark children of remote resource for update",
				"reason", err)
			continue
		}
		if err := gNode.Close(ctx); err != nil {
			resLogger.CErrorw(ctx,
				"failed to close remote resource node",
				"reason", err)
		}
		anythingChanged = true
	}
	return anythingChanged
}

func (manager *resourceManager) updateRemotesResourceNames(ctx context.Context) bool {
	anythingChanged := false
	for _, name := range manager.resources.Names() {
		gNode, _ := manager.resources.Node(name)
		if name.API == client.RemoteAPI {
			res, err := gNode.Resource()
			if err == nil {
				if rr, ok := res.(internalRemoteRobot); ok {
					// updateRemoteResourceNames must be first, otherwise there's a chance it will not be evaluated
					anythingChanged = manager.updateRemoteResourceNames(ctx, name, rr, false) || anythingChanged
				}
			}
		}
	}
	return anythingChanged
}

// RemoteNames returns the names of all available remotes in the manager.
func (manager *resourceManager) RemoteNames() []string {
	names := []string{}
	for _, k := range manager.resources.Names() {
		if k.API != client.RemoteAPI {
			continue
		}
		gNode, ok := manager.resources.Node(k)
		if !ok || !gNode.HasResource() {
			continue
		}
		names = append(names, k.Name)
	}
	return names
}

func (manager *resourceManager) anyResourcesNotConfigured() bool {
	for _, name := range manager.resources.Names() {
		res, ok := manager.resources.Node(name)
		if !ok {
			continue
		}
		if res.NeedsReconfigure() {
			return true
		}
	}
	return false
}

func (manager *resourceManager) internalResourceNames() []resource.Name {
	names := []resource.Name{}
	for _, k := range manager.resources.Names() {
		if k.API.Type.Namespace != resource.APINamespaceRDKInternal {
			continue
		}
		names = append(names, k)
	}
	return names
}

// ResourceNames returns the names of all resources in the manager, excluding the following types of resources:
// - Resources that represent entire remote machines.
// - Resources that are considered internal to viam-server that cannot be removed via configuration.
func (manager *resourceManager) ResourceNames() []resource.Name {
	names := []resource.Name{}
	for _, k := range manager.resources.Names() {
		if manager.resourceName(k) {
			names = append(names, k)
		}
	}
	return names
}

// reachableResourceNames returns the names of all resources in the manager, excluding the following types of resources:
// - Resources that represent entire remote machines.
// - Resources that are considered internal to viam-server that cannot be removed via configuration.
// - Remote resources that are currently unreachable.
func (manager *resourceManager) reachableResourceNames() []resource.Name {
	names := []resource.Name{}
	for _, k := range manager.resources.ReachableNames() {
		if manager.resourceName(k) {
			names = append(names, k)
		}
	}
	return names
}

// resourceName is a validation function that dictates if a given [resource.Name] should be returned by [ResourceNames].
// A resource should NOT be returned by [ResourceNames] if any of the following conditions are true:
// - The resource is not stored in the resource manager.
// - The resource represents an entire remote machine.
// - The resource is considered internal to viam-server, meaning it cannot be removed via configuration.
func (manager *resourceManager) resourceName(k resource.Name) bool {
	if k.API == client.RemoteAPI ||
		k.API.Type.Namespace == resource.APINamespaceRDKInternal {
		return false
	}
	gNode, ok := manager.resources.Node(k)
	if !ok || !gNode.HasResource() {
		return false
	}
	return true
}

// ResourceRPCAPIs returns the types of all resource RPC APIs in use by the manager.
func (manager *resourceManager) ResourceRPCAPIs() []resource.RPCAPI {
	resourceAPIs := resource.RegisteredAPIs()

	types := map[resource.API]*desc.ServiceDescriptor{}
	for _, k := range manager.resources.Names() {
		if k.API.Type.Namespace == resource.APINamespaceRDKInternal {
			continue
		}
		if k.API == client.RemoteAPI {
			gNode, ok := manager.resources.Node(k)
			if !ok || !gNode.HasResource() {
				continue
			}
			res, err := gNode.Resource()
			if err != nil {
				manager.logger.Errorw(
					"error getting remote from node",
					"remote",
					k.Name,
					"err",
					err,
				)
				continue
			}
			rr, ok := res.(robot.Robot)
			if !ok {
				manager.logger.Debugw(
					"remote does not implement robot interface",
					"remote",
					k.Name,
					"type",
					reflect.TypeOf(res),
				)
				continue
			}
			manager.mergeResourceRPCAPIsWithRemote(rr, types)
			continue
		}
		if k.ContainsRemoteNames() {
			continue
		}
		if types[k.API] != nil {
			continue
		}

		st, ok := resourceAPIs[k.API]
		if !ok {
			continue
		}

		if st.ReflectRPCServiceDesc != nil {
			types[k.API] = st.ReflectRPCServiceDesc
		}
	}
	typesList := make([]resource.RPCAPI, 0, len(types))
	for k, v := range types {
		typesList = append(typesList, resource.RPCAPI{
			API:  k,
			Desc: v,
		})
	}
	return typesList
}

// mergeResourceRPCAPIsWithRemotes merges types from the manager itself as well as its
// remotes.
func (manager *resourceManager) mergeResourceRPCAPIsWithRemote(r robot.Robot, types map[resource.API]*desc.ServiceDescriptor) {
	remoteTypes := r.ResourceRPCAPIs()
	for _, remoteType := range remoteTypes {
		if svcName, ok := types[remoteType.API]; ok {
			if svcName.GetFullyQualifiedName() != remoteType.Desc.GetFullyQualifiedName() {
				manager.logger.Errorw(
					"remote proto service name clashes with another of the same API",
					"existing", svcName.GetFullyQualifiedName(),
					"remote", remoteType.Desc.GetFullyQualifiedName())
			}
			continue
		}
		types[remoteType.API] = remoteType.Desc
	}
}

func (manager *resourceManager) closeResource(ctx context.Context, res resource.Resource) error {
	manager.logger.CInfow(ctx, "Now removing resource", "resource", res.Name())

	// TODO(RSDK-6626): We should be resilient to builtin resource `Close` calls
	// hanging and not respecting the context created below. We will likely need
	// a goroutine/timer setup here similar to that in the (re)configuration
	// code.
	//
	// Avoid hangs in Close/RemoveResource with resourceCloseTimeout.
	closeCtx, cancel := context.WithTimeout(ctx, resourceCloseTimeout)
	defer cancel()

	cleanup := rutils.SlowLogger(
		closeCtx,
		"Waiting for resource to close",
		"resource", res.Name().String(),
		manager.logger,
	)
	defer cleanup()

	allErrs := res.Close(closeCtx)

	resName := res.Name()
	if manager.moduleManager != nil && manager.moduleManager.IsModularResource(resName) {
		if err := manager.moduleManager.RemoveResource(closeCtx, resName); err != nil {
			allErrs = multierr.Combine(
				allErrs,
				fmt.Errorf("error removing modular resource for closure: %w, resource_name: %s", err, res.Name().String()),
			)
		}
	}

	return allErrs
}

// closeAndUnsetResource attempts to close and unset the resource from the graph node. Should only be called within
// resourceGraphLock.
func (manager *resourceManager) closeAndUnsetResource(ctx context.Context, gNode *resource.GraphNode) error {
	res, err := gNode.Resource()
	if err != nil {
		return err
	}
	err = manager.closeResource(ctx, res)

	// resource may fail to close, but even in that case, resource should be unset from the node
	gNode.UnsetResource()
	return err
}

// removeMarkedAndClose removes all resources marked for removal from the graph and
// also closes them. It accepts an excludeFromClose in case some marked resources were
// already removed (e.g. renamed resources that count as remove + add but need to close
// before add) or need to be removed in a different way (e.g. web internal service last).
func (manager *resourceManager) removeMarkedAndClose(
	ctx context.Context,
	excludeFromClose map[resource.Name]struct{},
) error {
	defer func() {
		if err := manager.viz.SaveSnapshot(manager.resources); err != nil {
			manager.logger.Warnw("failed to save graph snapshot", "error", err)
		}
	}()

	var allErrs error
	toClose := manager.resources.RemoveMarked()
	for _, res := range toClose {
		resName := res.Name()
		if _, ok := excludeFromClose[resName]; ok {
			continue
		}
		allErrs = multierr.Combine(allErrs, manager.closeResource(ctx, res))
	}
	return allErrs
}

// Close attempts to close/stop all parts.
func (manager *resourceManager) Close(ctx context.Context) error {
	manager.resources.MarkForRemoval(manager.resources.Clone())

	var allErrs error

	// our caller will close web
	excludeWebFromClose := map[resource.Name]struct{}{
		web.InternalServiceName: {},
	}
	if err := manager.removeMarkedAndClose(ctx, excludeWebFromClose); err != nil {
		allErrs = multierr.Combine(allErrs, err)
	}
	// take a lock minimally to make a copy of the moduleManager.
	manager.modManagerLock.Lock()
	modManager := manager.moduleManager
	manager.modManagerLock.Unlock()
	// moduleManager may be nil in tests, and must be closed last, after resources within have been closed properly above
	if modManager != nil {
		if err := modManager.Close(ctx); err != nil {
			allErrs = multierr.Combine(allErrs, fmt.Errorf("error closing module manager: %w", err))
		}
	}

	return allErrs
}

// Kill attempts to kill all module processes.
func (manager *resourceManager) Kill() {
	// TODO(RSDK-9709): Kill processes in processManager as well.

	// take a lock minimally to make a copy of the moduleManager.
	manager.modManagerLock.Lock()
	modManager := manager.moduleManager
	manager.modManagerLock.Unlock()
	// moduleManager may be nil in tests
	if modManager != nil {
		modManager.Kill()
	}
}

// completeConfig process the tree in reverse order and attempts to build or reconfigure
// resources that are wrapped in a placeholderResource. this function will attempt to
// process resources concurrently when they do not depend on each other unless
// `forceSynce` is set to true.
func (manager *resourceManager) completeConfig(
	ctx context.Context,
	lr *localRobot,
	forceSync bool,
) {
	defer func() {
		if err := manager.viz.SaveSnapshot(manager.resources); err != nil {
			manager.logger.Warnw("failed to save graph snapshot", "error", err)
		}
	}()

	// first handle remotes since they may reveal unresolved dependencies
	manager.completeConfigForRemotes(ctx, lr)

	// now resolve prior to sorting in case there's anything newly discovered
	if err := manager.resources.ResolveDependencies(manager.logger); err != nil {
		// debug here since the resolver will log on its own
		manager.logger.CDebugw(ctx, "error resolving dependencies", "error", err)
	}

	// sort resources into topological "levels" based on their dependencies. resources in
	// any given level only depend on resources in prior levels. this makes it safe to
	// process resources within a level concurrently as long as levels are processed in
	// order.
	levels := manager.resources.ReverseTopologicalSortInLevels()
	timeout := rutils.GetResourceConfigurationTimeout(manager.logger)
	for _, resourceNames := range levels {
		// At the start of every reconfiguration level, check if
		// updateWeakAndOptionalDependents should be run by checking if the logical clock is
		// higher than the `lastWeakAndOptionalDependentsRound` value.
		//
		// This will make sure that weak and optional dependents are updated before they are
		// passed into constructors or reconfigure methods.
		//
		// Resources that depend on weak or optional dependents should expect that the
		// weak/optional dependents passed into the constructor or reconfigure method will
		// only have been reconfigured with all resources constructed before their level.
		for _, resName := range resourceNames {
			select {
			case <-ctx.Done():
				return
			default:
			}
			gNode, ok := manager.resources.Node(resName)
			if !ok || !gNode.NeedsReconfigure() {
				continue
			}
			if !(resName.API.IsComponent() || resName.API.IsService()) {
				continue
			}

			if lr.lastWeakAndOptionalDependentsRound.Load() < manager.resources.CurrLogicalClockValue() {
				lr.updateWeakAndOptionalDependents(ctx)
			}
		}
		// we use an errgroup here instead of a normal waitgroup to conveniently bubble
		// up errors in resource processing goroutinues that warrant an early exit.
		var levelErrG errgroup.Group
		// Add resources in batches instead of all at once. We've observed this to be more
		// reliable when there are a large number of resources to add (e.g. hundreds).
		levelErrG.SetLimit(10)
		for _, resName := range resourceNames {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// processResource is intended to be run concurrently for each resource
			// within a topological sort level. if any processResource function returns a
			// non-nil error then the entire `completeConfig` function will exit early.
			//
			// currently only a top-level context cancellation will result in an early
			// exist - individual resource processing failures will not.
			processResource := func() error {
				resChan := make(chan struct{}, 1)
				ctxWithTimeout, timeoutCancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
				defer timeoutCancel()

				stopSlowLogger := rutils.SlowLogger(
					ctx, "Waiting for resource to complete (re)configuration", "resource", resName.String(), manager.logger)

				lr.reconfigureWorkers.Add(1)
				goutils.PanicCapturingGo(func() {
					defer func() {
						stopSlowLogger()
						resChan <- struct{}{}
						lr.reconfigureWorkers.Done()
					}()
					gNode, ok := manager.resources.Node(resName)
					if !ok || !gNode.NeedsReconfigure() {
						return
					}
					if !(resName.API.IsComponent() || resName.API.IsService()) {
						return
					}

					var prefix string
					conf := gNode.Config()
					if gNode.IsUninitialized() {
						gNode.InitializeLogger(
							manager.logger, resName.String(),
						)
					} else {
						prefix = "re"
					}
					manager.logger.CInfow(ctx, fmt.Sprintf("Now %sconfiguring resource", prefix), "resource", resName, "model", conf.Model)

					// The config was already validated, but we must check again before attempting
					// to add.
					if _, _, err := conf.Validate("", resName.API.Type.Name); err != nil {
						gNode.LogAndSetLastError(
							fmt.Errorf("resource config validation error: %w", err),
							"resource", conf.ResourceName(),
							"model", conf.Model)
						return
					}
					if manager.moduleManager.Provides(conf) {
						if _, _, err := manager.moduleManager.ValidateConfig(ctxWithTimeout, conf); err != nil {
							gNode.LogAndSetLastError(
								fmt.Errorf("modular resource config validation error: %w", err),
								"resource", conf.ResourceName(),
								"model", conf.Model)
							return
						}
					}

					switch {
					case resName.API.IsComponent(), resName.API.IsService():

						newRes, newlyBuilt, err := manager.processResource(ctxWithTimeout, conf, gNode, lr)
						if newlyBuilt || err != nil {
							if err := manager.markChildrenForUpdate(resName); err != nil {
								manager.logger.CErrorw(ctx,
									"failed to mark children of resource for update",
									"resource", resName,
									"reason", err)
							}
						}

						if err != nil {
							gNode.LogAndSetLastError(
								fmt.Errorf("resource build error: %w", err),
								"resource", conf.ResourceName(),
								"model", conf.Model)
							return
						}

						// if the ctxWithTimeout fails with DeadlineExceeded, then that means that
						// resource generation is running async, and we don't currently have good
						// validation around how this might affect the resource graph. So, we avoid
						// updating the graph to be safe.
						if errors.Is(ctxWithTimeout.Err(), context.DeadlineExceeded) {
							manager.logger.CErrorw(
								ctx, "error building resource", "resource", conf.ResourceName(), "model", conf.Model, "error", ctxWithTimeout.Err())
						} else {
							gNode.SwapResource(newRes, conf.Model, manager.opts.ftdc)
							manager.logger.CInfow(ctx, fmt.Sprintf("Successfully %sconfigured resource", prefix), "resource", resName, "model", conf.Model)
						}

					default:
						err := errors.New("config is not for a component or service")
						gNode.LogAndSetLastError(err, "resource", resName)
					}
				})

				select {
				case <-resChan:
				case <-ctxWithTimeout.Done():
					// this resource is taking too long to process, so we give up but
					// continue processing other resources. we do not wait for this
					// resource to finish processing since it may be running outside code
					// and have unexpected behavior.
					if errors.Is(ctxWithTimeout.Err(), context.DeadlineExceeded) {
						lr.logger.CWarn(ctx, rutils.NewBuildTimeoutError(resName.String(), lr.logger))
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			}

			syncRes := forceSync
			if !syncRes {
				// TODO(RSDK-6925): support concurrent processing of resources of
				// APIs with a maximum instance limit. Currently this limit is
				// validated later in the resource creation flow and assumes that
				// each resource is created synchronously to have an accurate
				// creation count.
				if c, ok := resource.LookupGenericAPIRegistration(resName.API); ok && c.MaxInstance != 0 {
					syncRes = true
				}
			}

			if syncRes {
				if err := processResource(); err != nil {
					return
				}
			} else {
				lr.reconfigureWorkers.Add(1)
				levelErrG.Go(func() error {
					defer lr.reconfigureWorkers.Done()
					return processResource()
				})
			}
		} // for-each resource name
		if err := levelErrG.Wait(); err != nil {
			return
		}
	} // for-each level
}

func (manager *resourceManager) completeConfigForRemotes(ctx context.Context, lr *localRobot) {
	// Add remotes in parallel. This is particularly useful in cases where
	// there are many remotes that are offline or slow to start up.
	var remoteErrGroup errgroup.Group
	remoteErrGroup.SetLimit(5)
	for _, resName := range manager.resources.FindNodesByAPI(client.RemoteAPI) {
		gNode, ok := manager.resources.Node(resName)
		if !ok || !gNode.NeedsReconfigure() {
			continue
		}
		processAndCompleteConfigForRemote := func() {
			var verb string
			if gNode.IsUninitialized() {
				verb = "configuring"
			} else {
				verb = "reconfiguring"
			}
			manager.logger.CInfow(ctx, fmt.Sprintf("Now %s a remote", verb), "resource", resName)
			switch resName.API {
			case client.RemoteAPI:
				remConf, err := resource.NativeConfig[*config.Remote](gNode.Config())
				if err != nil {
					manager.logger.CErrorw(ctx, "remote config error", "error", err)
					return
				}
				if gNode.IsUninitialized() {
					gNode.InitializeLogger(
						manager.logger, fromRemoteNameToRemoteNodeName(remConf.Name).String(),
					)
				}
				// The config was already validated, but we must check again before attempting
				// to add.
				if _, _, err := remConf.Validate(""); err != nil {
					gNode.LogAndSetLastError(
						fmt.Errorf("remote config validation error: %w", err), "remote", remConf.Name)
					return
				}
				rr, err := manager.processRemote(ctx, *remConf, gNode)
				if err != nil {
					gNode.LogAndSetLastError(
						fmt.Errorf("error connecting to remote: %w", err), "remote", remConf.Name)
					return
				}
				manager.addRemote(ctx, rr, gNode, *remConf)
				rr.SetParentNotifier(func() {
					lr.sendTriggerConfig(remConf.Name)
				})
			default:
				err := errors.New("config is not a remote config")
				manager.logger.CErrorw(ctx, err.Error(), "resource", resName)
			}
		}
		lr.reconfigureWorkers.Add(1)
		remoteErrGroup.Go(func() error {
			defer lr.reconfigureWorkers.Done()
			processAndCompleteConfigForRemote()
			return nil
		})
	}
	if err := remoteErrGroup.Wait(); err != nil {
		return
	}
}

// cleanAppImageEnv attempts to revert environment variable changes so
// normal, non-AppImage processes can be executed correctly.
func cleanAppImageEnv() error {
	_, isAppImage := os.LookupEnv("APPIMAGE")
	if isAppImage {
		err := os.Chdir(os.Getenv("APPRUN_CWD"))
		if err != nil {
			return err
		}

		// Reset original values where available
		for _, eVarStr := range os.Environ() {
			eVar := strings.Split(eVarStr, "=")
			key := eVar[0]
			origV, present := os.LookupEnv("APPRUN_ORIGINAL_" + key)
			if present {
				if origV != "" {
					err = os.Setenv(key, origV)
				} else {
					err = os.Unsetenv(key)
				}
				if err != nil {
					return err
				}
			}
		}

		// Remove all explicit appimage vars
		err = multierr.Combine(os.Unsetenv("ARGV0"), os.Unsetenv("ORIGIN"))
		for _, eVarStr := range os.Environ() {
			eVar := strings.Split(eVarStr, "=")
			key := eVar[0]
			if strings.HasPrefix(key, "APPRUN") ||
				strings.HasPrefix(key, "APPDIR") ||
				strings.HasPrefix(key, "APPIMAGE") ||
				strings.HasPrefix(key, "AIX_") {
				err = multierr.Combine(err, os.Unsetenv(key))
			}
		}
		if err != nil {
			return err
		}

		// Remove AppImage paths from path-like env vars
		for _, eVarStr := range os.Environ() {
			eVar := strings.Split(eVarStr, "=")
			var newPaths []string
			const mountPrefix = "/tmp/.mount_"
			key := eVar[0]
			if len(eVar) >= 2 && strings.Contains(eVar[1], mountPrefix) {
				for _, path := range strings.Split(eVar[1], ":") {
					if !strings.HasPrefix(path, mountPrefix) && path != "" {
						newPaths = append(newPaths, path)
					}
				}
				if len(newPaths) > 0 {
					err = os.Setenv(key, strings.Join(newPaths, ":"))
				} else {
					err = os.Unsetenv(key)
				}
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// newRemotes constructs all remotes defined and integrates their parts in.
func (manager *resourceManager) processRemote(
	ctx context.Context,
	config config.Remote,
	gNode *resource.GraphNode,
) (*client.RobotClient, error) {
	// if there was an existing client (i.e. remote was modified), close old client before making a new one
	res, err := gNode.Resource()
	if err == nil {
		err = res.Close(ctx)
		if err != nil {
			return nil, err
		}
	}

	dialOpts := remoteDialOptions(config, manager.opts)
	manager.logger.CInfow(ctx, "Connecting now to remote", "remote", config.Name)
	robotClient, err := dialRobotClient(ctx, config, gNode.Logger(), dialOpts...)
	if err != nil {
		if errors.Is(err, rpc.ErrInsecureWithCredentials) {
			if manager.opts.fromCommand {
				err = errors.New("must use -allow-insecure-creds flag to connect to a non-TLS secured robot")
			} else {
				err = errors.New("must use Config.AllowInsecureCreds to connect to a non-TLS secured robot")
			}
		}
		return nil, fmt.Errorf("couldn't connect to robot remote (%s): %w", config.Address, err)
	}
	manager.logger.CInfow(ctx, "Connected now to remote", "remote", config.Name)
	return robotClient, nil
}

// RemoteByName returns the given remote robot by name, if it exists;
// returns nil otherwise.
func (manager *resourceManager) RemoteByName(name string) (internalRemoteRobot, bool) {
	rName := resource.NewName(client.RemoteAPI, name)
	if gNode, ok := manager.resources.Node(rName); ok {
		remRes, err := gNode.Resource()
		if err != nil {
			manager.logger.Errorw("error getting remote", "remote", name, "err", err)
			return nil, false
		}
		remRobot, ok := remRes.(internalRemoteRobot)
		if !ok {
			manager.logger.Errorw("tried to access remote but its not a robot interface", "remote", name, "type", reflect.TypeOf(remRes))
		}
		return remRobot, ok
	}
	return nil, false
}

func (manager *resourceManager) markChildrenForUpdate(rName resource.Name) error {
	sg, err := manager.resources.SubGraphFrom(rName)
	if err != nil {
		return err
	}
	sorted := sg.TopologicalSort()
	for _, name := range sorted {
		if name == rName || name.ContainsRemoteNames() {
			continue // ignore self and non-local resources
		}
		gNode, ok := manager.resources.Node(name)
		if !ok {
			continue
		}
		gNode.SetNeedsUpdate()
	}
	return nil
}

func (manager *resourceManager) processResource(
	ctx context.Context,
	conf resource.Config,
	gNode *resource.GraphNode,
	lr *localRobot,
) (resource.Resource, bool, error) {
	if gNode.IsUninitialized() {
		newRes, err := lr.newResource(ctx, gNode, conf)
		if err != nil {
			return nil, false, err
		}
		return newRes, true, nil
	}

	currentRes, err := gNode.UnsafeResource()
	if err != nil {
		return nil, false, err
	}

	resName := conf.ResourceName()
	deps, err := lr.getDependencies(resName, gNode)
	if err != nil {
		manager.logger.CDebugw(ctx,
			"failed to get dependencies for existing resource during reconfiguration, closing and removing resource from graph node",
			"name", resName,
			"old_model", gNode.ResourceModel(),
			"new_model", conf.Model,
		)
		return nil, false, multierr.Combine(err, manager.closeAndUnsetResource(ctx, gNode))
	}

	isModular := manager.moduleManager.Provides(conf)
	if gNode.ResourceModel() == conf.Model {
		if isModular {
			if err := manager.moduleManager.ReconfigureResource(ctx, conf, modmanager.DepsToNames(deps)); err != nil {
				return nil, false, err
			}
			return currentRes, false, nil
		}

		err = currentRes.Reconfigure(ctx, deps, conf)
		if err == nil {
			return currentRes, false, nil
		}

		if !resource.IsMustRebuildError(err) {
			return nil, false, err
		}
	} else {
		manager.logger.CInfow(ctx, "Resource models differ so resource must be rebuilt",
			"name", resName, "old_model", gNode.ResourceModel(), "new_model", conf.Model)
	}

	manager.logger.CDebugw(
		ctx,
		"rebuilding resource, closing and removing existing resource from graph node",
		"name", resName,
		"old_model", gNode.ResourceModel(),
		"new_model", conf.Model,
	)
	if err := lr.manager.closeAndUnsetResource(ctx, gNode); err != nil {
		manager.logger.CError(ctx, err)
	}
	newRes, err := lr.newResource(ctx, gNode, conf)
	if err != nil {
		manager.logger.CDebugw(ctx,
			"failed to build resource of new model",
			"name", resName,
			"old_model", gNode.ResourceModel(),
			"new_model", conf.Model,
		)
		return nil, false, err
	}
	return newRes, true, nil
}

// markResourceForUpdate marks the given resource in the graph to be updated. If it does not exist, a new node
// is inserted. If it does exist, it's properly marked. Once this is done, all information needed to build/reconfigure
// will be available when we call completeConfig.
func (manager *resourceManager) markResourceForUpdate(name resource.Name, conf resource.Config, deps []string, revision string) error {
	gNode, hasNode := manager.resources.Node(name)
	if hasNode {
		gNode.SetNewConfig(conf, deps)
		gNode.UpdatePendingRevision(revision)
		// reset parentage
		for _, parent := range manager.resources.GetAllParentsOf(name) {
			manager.resources.RemoveChild(name, parent)
		}
		return nil
	}
	gNode = resource.NewUnconfiguredGraphNode(conf, deps)
	gNode.UpdatePendingRevision(revision)
	if err := manager.resources.AddNode(name, gNode); err != nil {
		return fmt.Errorf("failed to add new node for unconfigured resource %q: %w", name, err)
	}
	return nil
}

// updateRevision updates the current revision of a node.
func (manager *resourceManager) updateRevision(name resource.Name, revision string) {
	if gNode, hasNode := manager.resources.Node(name); hasNode {
		gNode.UpdateRevision(revision)
	}
}

// updateResources will use the difference between the current config
// and next one to create resource nodes with configs that completeConfig will later on use.
// Ideally at the end of this function we should have a complete graph representation of the configuration
// for all well known resources. For resources that cannot be matched up to their dependencies, they will
// be in an unresolved state for later resolution.
func (manager *resourceManager) updateResources(
	ctx context.Context,
	conf *config.Diff,
) error {
	var allErrs error

	// modules are not added into the resource tree as they belong to the module manager
	if conf.Added.Modules != nil {
		if err := manager.moduleManager.Add(ctx, conf.Added.Modules...); err != nil {
			manager.logger.CErrorw(ctx, "error adding modules", "error", err)
		}
	}

	for _, mod := range conf.Modified.Modules {
		// The config was already validated, but we must check again before attempting
		// to reconfigure.
		if err := mod.Validate(""); err != nil {
			manager.logger.CErrorw(ctx, "module config validation error; skipping", "module", mod.Name, "error", err)
			continue
		}
		affectedResourceNames, err := manager.moduleManager.Reconfigure(ctx, mod)
		if err != nil {
			manager.logger.CErrorw(ctx, "error reconfiguring module", "module", mod.Name, "error", err)
		}
		// resources passed into markRebuildResources have already been closed during module reconfiguration, so
		// not necessary to Close again.
		manager.markRebuildResources(affectedResourceNames)
	}

	if manager.moduleManager != nil {
		if err := manager.moduleManager.ResolveImplicitDependenciesInConfig(ctx, conf); err != nil {
			manager.logger.CErrorw(ctx, "error adding implicit dependencies", "error", err)
		}
	}

	revision := conf.NewRevision()
	for _, s := range conf.Added.Services {
		rName := s.ResourceName()
		if manager.opts.untrustedEnv && rName.API == shell.API {
			allErrs = multierr.Combine(allErrs, errShellServiceDisabled)
			continue
		}
		markErr := manager.markResourceForUpdate(rName, s, s.Dependencies(), revision)
		allErrs = multierr.Combine(allErrs, markErr)
	}
	for _, c := range conf.Added.Components {
		rName := c.ResourceName()
		markErr := manager.markResourceForUpdate(rName, c, c.Dependencies(), revision)
		allErrs = multierr.Combine(allErrs, markErr)
	}
	for _, r := range conf.Added.Remotes {
		rName := fromRemoteNameToRemoteNodeName(r.Name)
		rCopy := r
		markErr := manager.markResourceForUpdate(rName, resource.Config{ConvertedAttributes: &rCopy}, []string{}, revision)
		allErrs = multierr.Combine(allErrs, markErr)
	}
	for _, c := range conf.Modified.Components {
		rName := c.ResourceName()
		markErr := manager.markResourceForUpdate(rName, c, c.Dependencies(), revision)
		allErrs = multierr.Combine(allErrs, markErr)
	}
	for _, s := range conf.Modified.Services {
		rName := s.ResourceName()

		// Disable shell service when in untrusted env
		if manager.opts.untrustedEnv && rName.API == shell.API {
			allErrs = multierr.Combine(allErrs, errShellServiceDisabled)
			continue
		}

		markErr := manager.markResourceForUpdate(rName, s, s.Dependencies(), revision)
		allErrs = multierr.Combine(allErrs, markErr)
	}
	for _, r := range conf.Modified.Remotes {
		rName := fromRemoteNameToRemoteNodeName(r.Name)
		rCopy := r
		markErr := manager.markResourceForUpdate(rName, resource.Config{ConvertedAttributes: &rCopy}, []string{}, revision)
		allErrs = multierr.Combine(allErrs, markErr)
	}

	if len(conf.Added.Processes) > 0 || len(conf.Modified.Processes) > 0 {
		manager.logger.CErrorw(ctx, "Processes have been deprecated and are no longer supported in viam-server versions v0.74.0+. "+
			"The processes config of this machine part has been ignored.")
	}

	return allErrs
}

// ResourceByName returns the given resource by fully qualified name, if it exists;
// returns an error otherwise.
func (manager *resourceManager) ResourceByName(name resource.Name) (resource.Resource, error) {
	if gNode, ok := manager.resources.Node(name); ok {
		res, err := gNode.Resource()
		if err != nil {
			return nil, resource.NewNotAvailableError(name, err)
		}
		return res, nil
	}
	// if we haven't found a resource of this name then we are going to look into remote resources to find it.
	// This is kind of weird and arguably you could have a ResourcesByPartialName that would match against
	// a string and not a resource name (e.g. expressions).
	if !name.ContainsRemoteNames() {
		keys := manager.resources.FindNodesByShortNameAndAPI(name)
		if len(keys) > 1 {
			return nil, rutils.NewRemoteResourceClashError(name.Name)
		}
		if len(keys) == 1 {
			gNode, ok := manager.resources.Node(keys[0])
			if ok {
				res, err := gNode.Resource()
				if err != nil {
					return nil, resource.NewNotAvailableError(name, err)
				}
				return res, nil
			}
		}
	}
	return nil, resource.NewNotFoundError(name)
}

// PartsMergeResult is the result of merging in parts together.
type PartsMergeResult struct {
	ReplacedProcesses []pexec.ManagedProcess
}

// markRemoved marks all resources in the config (assumed to be a removed diff) for removal. This must be called
// before updateResources. After updateResources is called, any resources still marked will be fully removed from
// the graph and closed. markRemoved also returns a list of resources to be rebuilt.
func (manager *resourceManager) markRemoved(
	ctx context.Context,
	conf *config.Config,
) ([]resource.Resource, map[resource.Name]struct{}, []resource.Name) {
	var resourcesToMark, resourcesToRebuild []resource.Name
	for _, conf := range conf.Modules {
		affectedResourceNames, err := manager.moduleManager.Remove(conf.Name)
		if err != nil {
			manager.logger.CErrorw(ctx, "error removing module", "module", conf.Name, "error", err)
		}
		resourcesToRebuild = append(resourcesToRebuild, affectedResourceNames...)
	}

	for _, conf := range conf.Remotes {
		resourcesToMark = append(
			resourcesToMark,
			fromRemoteNameToRemoteNodeName(conf.Name),
		)
	}
	for _, conf := range append(conf.Components, conf.Services...) {
		resourcesToMark = append(resourcesToMark, conf.ResourceName())
	}
	markedResourceNames := map[resource.Name]struct{}{}
	addNames := func(names ...resource.Name) {
		for _, name := range names {
			markedResourceNames[name] = struct{}{}
		}
	}
	// if the resource was directly removed, remove its dependents as well, since their parents will
	// be removed.
	resourcesToCloseBeforeComplete := manager.markResourcesRemoved(resourcesToMark, addNames, true)

	// for modular resources that are being removed because the underlying module was removed,
	// we only want to mark the resources for removal, but not its dependents. They will be marked
	// for update later in the process.
	resourcesToCloseBeforeComplete = append(
		resourcesToCloseBeforeComplete,
		manager.markResourcesRemoved(resourcesToRebuild, addNames, false)...)
	return resourcesToCloseBeforeComplete, markedResourceNames, resourcesToRebuild
}

// markResourcesRemoved marks all passed in resources (assumed to be resource
// names of components, services or remotes) for removal.
func (manager *resourceManager) markResourcesRemoved(
	rNames []resource.Name,
	addNames func(names ...resource.Name),
	withDependents bool,
) []resource.Resource {
	var resourcesToCloseBeforeComplete []resource.Resource
	for _, rName := range rNames {
		// Disable changes to shell in untrusted
		if manager.opts.untrustedEnv && rName.API == shell.API {
			continue
		}

		resNode, ok := manager.resources.Node(rName)
		if !ok {
			continue
		}
		resourcesToCloseBeforeComplete = append(resourcesToCloseBeforeComplete,
			resource.NewCloseOnlyResource(rName, resNode.Close))
		resNode.MarkForRemoval()

		if withDependents {
			subG, err := manager.resources.SubGraphFrom(rName)
			if err != nil {
				manager.logger.Errorw("error while getting a subgraph", "error", err)
				continue
			}
			if addNames != nil {
				addNames(subG.Names()...)
			}
			manager.resources.MarkForRemoval(subG)
		}
	}
	return resourcesToCloseBeforeComplete
}

// markRebuildResources marks resources passed in as needing a rebuild during
// reconfiguration and/or completeConfig loop. This function expects the caller
// to close any resources if necessary.
func (manager *resourceManager) markRebuildResources(rNames []resource.Name) {
	for _, rName := range rNames {
		// Disable changes to shell in untrusted
		if manager.opts.untrustedEnv && rName.API == shell.API {
			continue
		}

		resNode, ok := manager.resources.Node(rName)
		if !ok {
			continue
		}
		resNode.SetNeedsRebuild()
		if err := manager.markChildrenForUpdate(rName); err != nil {
			manager.logger.Errorw("error marking children for update", "resource", rName, "error", err)
		}
	}
}

// createConfig will create a config.Config based on the current state of the
// resource graph, processManager and moduleManager. The created config will
// possibly contain default services registered by the RDK and not specified by
// the user in their config.
func (manager *resourceManager) createConfig() *config.Config {
	conf := &config.Config{}

	for _, resName := range manager.resources.Names() {
		// Ignore non-local resources.
		if resName.ContainsRemoteNames() {
			continue
		}
		gNode, ok := manager.resources.Node(resName)
		if !ok {
			continue
		}
		resConf := gNode.Config()

		// gocritic will complain that this if-else chain should be a switch, but
		// it's really a mix of == and bool method checks.
		//
		//nolint: gocritic
		if resName.API == client.RemoteAPI {
			remoteConf, err := rutils.AssertType[*config.Remote](resConf.ConvertedAttributes)
			if err != nil {
				manager.logger.Errorw("error getting remote config",
					"remote", resName.String(),
					"error", err)
				continue
			}

			conf.Remotes = append(conf.Remotes, *remoteConf)
		} else if resName.API.IsComponent() {
			conf.Components = append(conf.Components, resConf)
		} else if resName.API.IsService() &&
			resName.API.Type.Namespace != resource.APINamespaceRDKInternal {
			conf.Services = append(conf.Services, resConf)
		}
	}

	conf.Modules = append(conf.Modules, manager.moduleManager.Configs()...)

	return conf
}

func remoteDialOptions(config config.Remote, opts resourceManagerOptions) []rpc.DialOption {
	var dialOpts []rpc.DialOption
	if opts.debug {
		dialOpts = append(dialOpts, rpc.WithDialDebug())
	}
	if config.Insecure {
		dialOpts = append(dialOpts, rpc.WithInsecure())
	}
	if opts.allowInsecureCreds {
		dialOpts = append(dialOpts, rpc.WithAllowInsecureWithCredentialsDowngrade())
	}
	if opts.tlsConfig != nil {
		dialOpts = append(dialOpts, rpc.WithTLSConfig(opts.tlsConfig))
	}
	if config.Auth.Credentials != nil {
		if config.Auth.Entity == "" {
			dialOpts = append(dialOpts, rpc.WithCredentials(*config.Auth.Credentials))
		} else {
			dialOpts = append(dialOpts, rpc.WithEntityCredentials(config.Auth.Entity, *config.Auth.Credentials))
		}
	} else {
		// explicitly unset credentials so they are not fed to remotes unintentionally.
		dialOpts = append(dialOpts, rpc.WithEntityCredentials("", rpc.Credentials{}))
	}

	if config.Auth.ExternalAuthAddress != "" {
		dialOpts = append(dialOpts, rpc.WithExternalAuth(
			config.Auth.ExternalAuthAddress,
			config.Auth.ExternalAuthToEntity,
		))
	}

	if config.Auth.ExternalAuthInsecure {
		dialOpts = append(dialOpts, rpc.WithExternalAuthInsecure())
	}

	if config.Auth.SignalingServerAddress != "" {
		wrtcOpts := rpc.DialWebRTCOptions{
			Config:                 &rpc.DefaultWebRTCConfiguration,
			SignalingServerAddress: config.Auth.SignalingServerAddress,
			SignalingAuthEntity:    config.Auth.SignalingAuthEntity,
		}
		if config.Auth.SignalingCreds != nil {
			wrtcOpts.SignalingCreds = *config.Auth.SignalingCreds
		}
		dialOpts = append(dialOpts, rpc.WithWebRTCOptions(wrtcOpts))

		if config.Auth.Managed {
			// managed robots use TLS authN/Z
			dialOpts = append(dialOpts, rpc.WithDialMulticastDNSOptions(rpc.DialMulticastDNSOptions{
				RemoveAuthCredentials: true,
			}))
		}
	}
	return dialOpts
}

// defaultRemoteMachineStatusTimeout is the default timeout for getting resource statuses from remotes. This prevents
// remote cycles from preventing this call from finishing.
var defaultRemoteMachineStatusTimeout = time.Minute

func (manager *resourceManager) getRemoteResourceMetadata(ctx context.Context) map[resource.Name]cloud.Metadata {
	resourceStatusMap := make(map[resource.Name]cloud.Metadata)
	for _, resName := range manager.resources.FindNodesByAPI(client.RemoteAPI) {
		gNode, _ := manager.resources.Node(resName)
		res, err := gNode.Resource()
		if err != nil {
			manager.logger.Debugw("error getting remote machine node", "remote", resName.Name, "err", err)
			continue
		}
		ctx, cancel := contextutils.ContextWithTimeoutIfNoDeadline(ctx, defaultRemoteMachineStatusTimeout)
		defer cancel()
		remote := res.(internalRemoteRobot)
		md, err := remote.CloudMetadata(ctx)
		if err != nil {
			manager.logger.Debugw("error getting remote cloud metadata", "remote", resName.Name, "err", err)
		}
		resourceStatusMap[resName] = md
		machineStatus, err := remote.MachineStatus(ctx)
		if err != nil {
			manager.logger.Debugw("error getting remote machine status", "remote", resName.Name, "err", err)
			continue
		}
		// Resources come back without their remote name since they are grabbed
		// from the remote themselves. We need to add that information back.
		//
		// Resources on remote may have different cloud metadata from each other, so keep a map of every
		// resource to cloud metadata pair we come across.
		for _, remoteResource := range machineStatus.Resources {
			nameWithRemote := remoteResource.Name.PrependRemote(resName.Name)
			resourceStatusMap[nameWithRemote] = remoteResource.CloudMetadata
		}
	}
	return resourceStatusMap
}
