package kubernetes

import (
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/zalando/skipper/eskip"
	"github.com/zalando/skipper/loadbalancer"
)

// TODO:
// - verify the uniqueness of the list fields, not only the method
// - verify that routes are copied when duplicated
// - review whether it can crash on malformed input
// - review errors, error reporting and logging
// - review and document which errors prevent load and load updates, and which ones are only logged
// - document in the CRD that the service type must be ClusterIP when using service backends
// - reconsider implicit routes: do we need them? They have a double behavior this way
// - document the implicit routes, or clarify: spec.routes is not optional, but the minimal example doesn't have
// any
// - document the rules and the loopholes with the host catch-all routes
// - document the behavior of the weight implementation

type routeGroups struct {
	options Options
}

type routeGroupContext struct {
	clusterState          *clusterState
	defaultFilters        defaultFilters
	routeGroup            *routeGroupItem
	hostRx                string
	hostRoutes            map[string][]*eskip.Route
	hasEastWestHost       bool
	eastWestEnabled       bool
	eastWestDomain        string
	provideHTTPSRedirect  bool
	httpsRedirectCode     int
	backendsByName        map[string]*skipperBackend
	defaultBackendTraffic map[string]float64
}

type routeContext struct {
	group      *routeGroupContext
	groupRoute *routeSpec
	id         string
	weight     int
	method     string
	backend    *skipperBackend
}

func newRouteGroups(o Options) *routeGroups {
	return &routeGroups{options: o}
}

func invalidBackendRef(rg *routeGroupItem, name string) error {
	return fmt.Errorf(
		"invalid backend reference in routegroup/%s/%s: %s",
		namespaceString(rg.Metadata.Namespace),
		rg.Metadata.Name,
		name,
	)
}

func notSupportedServiceType(s *service) error {
	return fmt.Errorf(
		"not supported service type in service/%s/%s: %s",
		namespaceString(s.Meta.Namespace),
		s.Meta.Name,
		s.Spec.Type,
	)
}

func notImplemented(a ...interface{}) error {
	return fmt.Errorf("not implemented: %v", fmt.Sprint(a...))
}

func hasEastWestHost(eastWestPostfix string, hosts []string) bool {
	for _, h := range hosts {
		if strings.HasSuffix(h, eastWestPostfix) {
			return true
		}
	}

	return false
}

func toSymbol(p string) string {
	b := []byte(p)
	for i := range b {
		if b[i] == '_' ||
			b[i] >= '0' && b[i] <= '9' ||
			b[i] >= 'a' && b[i] <= 'z' ||
			b[i] >= 'A' && b[i] <= 'Z' {
			continue
		}

		b[i] = '_'
	}

	return string(b)
}

func rgRouteID(namespace, name, subName string, index, subIndex int) string {
	return fmt.Sprintf(
		"kube_rg__%s__%s__%s__%d_%d",
		namespace,
		name,
		subName,
		index,
		subIndex,
	)
}

func crdRouteID(m *metadata, method string, routeIndex, backendIndex int) string {
	return rgRouteID(
		toSymbol(namespaceString(m.Namespace)),
		toSymbol(m.Name),
		toSymbol(method),
		routeIndex,
		backendIndex,
	)
}

func mapBackends(backends []*skipperBackend) map[string]*skipperBackend {
	m := make(map[string]*skipperBackend)
	for _, b := range backends {
		m[b.Name] = b
	}

	return m
}

// calculateTraffic calculates the traffic values for the skipper Traffic() predicates
// based on the weight values in the backend references. It represents the remainder
// traffic as 1, where no Traffic predicate is meant to be set.
func calculateTraffic(b []*backendReference) map[string]float64 {
	var sum int
	weights := make([]int, len(b))
	for i, bi := range b {
		// TODO: validate no negative
		sum += bi.Weight
		weights[i] = bi.Weight
	}

	if sum == 0 {
		sum = len(weights)
		for i := range weights {
			weights[i] = 1
		}
	}

	traffic := make(map[string]float64)
	for i, bi := range b {
		if sum == 0 {
			traffic[bi.BackendName] = 1
			break
		}

		traffic[bi.BackendName] = float64(weights[i]) / float64(sum)
		sum -= weights[i]
	}

	return traffic
}

func applyDefaultFilters(ctx *routeGroupContext, serviceName string, r *eskip.Route) error {
	f, err := ctx.defaultFilters.getNamed(ctx.routeGroup.Metadata.Namespace, serviceName)
	if err != nil {
		return err
	}

	// safe to prepend as defaultFilters.get() copies the slice:
	r.Filters = append(f, r.Filters...)
	return nil
}

func getBackendService(ctx *routeGroupContext, backend *skipperBackend) (*service, error) {
	if backend.ServiceName == "" || backend.ServicePort <= 0 {
		return nil, fmt.Errorf(
			"invalid service backend in routegroup/%s/%s: %s:%d",
			namespaceString(ctx.routeGroup.Metadata.Namespace),
			ctx.routeGroup.Metadata.Name,
			backend.ServiceName,
			backend.ServicePort,
		)
	}

	s, err := ctx.clusterState.getService(
		namespaceString(ctx.routeGroup.Metadata.Namespace),
		backend.ServiceName,
	)
	if err != nil {
		return nil, err
	}

	if strings.ToLower(s.Spec.Type) != "clusterip" {
		return nil, notSupportedServiceType(s)
	}

	for _, p := range s.Spec.Ports {
		if p == nil {
			continue
		}

		if p.Port == backend.ServicePort {
			return s, nil
		}
	}

	return nil, fmt.Errorf(
		"service port not found for routegroup/%s/%s: %d",
		namespaceString(ctx.routeGroup.Metadata.Namespace),
		ctx.routeGroup.Metadata.Name,
		backend.ServicePort,
	)
}

func createClusterIPBackend(s *service, backend *skipperBackend) string {
	return fmt.Sprintf("http://%s:%d", s.Spec.ClusterIP, backend.ServicePort)
}

func applyServiceBackend(ctx *routeGroupContext, backend *skipperBackend, r *eskip.Route) error {
	s, err := getBackendService(ctx, backend)
	if err != nil {
		return err
	}

	targetPort, ok := s.getTargetPortByValue(backend.ServicePort)
	if !ok {
		// TODO: log fallback
		r.BackendType = eskip.NetworkBackend
		r.Backend = createClusterIPBackend(s, backend)
		return err
	}

	eps := ctx.clusterState.getEndpointsByTarget(
		namespaceString(ctx.routeGroup.Metadata.Namespace),
		s.Meta.Name,
		targetPort,
	)

	if len(eps) == 0 {
		// TODO: log fallback
		r.BackendType = eskip.NetworkBackend
		r.Backend = createClusterIPBackend(s, backend)
		return nil
	}

	r.BackendType = eskip.LBBackend
	r.LBEndpoints = eps
	r.LBAlgorithm = defaultLoadBalancerAlgorithm
	if backend.Algorithm != loadbalancer.None {
		r.LBAlgorithm = backend.Algorithm.String()
	}

	return nil
}

func applyBackend(ctx *routeGroupContext, backend *skipperBackend, r *eskip.Route) error {
	r.BackendType = backend.Type
	switch r.BackendType {
	case serviceBackend:
		if err := applyServiceBackend(ctx, backend, r); err != nil {
			return err
		}

		if err := applyDefaultFilters(ctx, backend.ServiceName, r); err != nil {
			log.Errorf("Failed to retrieve default filters: %v.", err)
		}
	case eskip.NetworkBackend:
		r.Backend = backend.Address
	case eskip.LBBackend:
		r.LBEndpoints = backend.Endpoints
		r.LBAlgorithm = defaultLoadBalancerAlgorithm
		r.LBAlgorithm = backend.Algorithm.String()
	default:
		return notImplemented("backend type", r.BackendType)
	}

	return nil
}

func appendPredicate(p []*eskip.Predicate, name string, args ...interface{}) []*eskip.Predicate {
	return append(p, &eskip.Predicate{
		Name: name,
		Args: args,
	})
}

func storeHostRoute(ctx *routeGroupContext, r *eskip.Route) {
	for _, h := range ctx.routeGroup.Spec.Hosts {
		ctx.hostRoutes[h] = append(ctx.hostRoutes[h], r)
	}
}

func appendEastWest(ctx *routeGroupContext, routes []*eskip.Route, current *eskip.Route) []*eskip.Route {
	// how will the route group name for the domain name play together with
	// zalando.org/v1/stackset and zalando.org/v1/fabricgateway? Wouldn't it be better to
	// use the service name instead?

	if !ctx.eastWestEnabled || ctx.hasEastWestHost {
		return routes
	}

	ewr := createEastWestRouteRG(
		ctx.routeGroup.Metadata.Name,
		namespaceString(ctx.routeGroup.Metadata.Namespace),
		ctx.eastWestDomain,
		current,
	)

	return append(routes, ewr)
}

func appendHTTPSRedirect(ctx *routeGroupContext, routes []*eskip.Route, current *eskip.Route) []*eskip.Route {
	// in case a route explicitly handles the forwarded proto header, we
	// don't shadow it

	if ctx.provideHTTPSRedirect && !hasProtoPredicate(current) {
		hsr := createHTTPSRedirect(ctx.httpsRedirectCode, current)
		routes = append(routes, hsr)
	}

	return routes
}

// implicitGroupRoutes creates routes for those route groups where the `route`
// field is not defined, and the routes are derived from the default backends.
func implicitGroupRoutes(ctx *routeGroupContext) ([]*eskip.Route, error) {
	rg := ctx.routeGroup
	if len(rg.Spec.DefaultBackends) == 0 {
		return nil, fmt.Errorf("missing route spec for route group: %s", rg.Metadata.Name)
	}

	var routes []*eskip.Route
	for backendIndex, beref := range rg.Spec.DefaultBackends {
		if beref == nil {
			log.Errorf(
				"Invalid default backend reference found in: routegroup/%s/%s.",
				namespaceString(rg.Metadata.Namespace),
				rg.Metadata.Name,
			)

			continue
		}

		be, ok := ctx.backendsByName[beref.BackendName]
		if !ok {
			return nil, invalidBackendRef(rg, beref.BackendName)
		}

		rid := crdRouteID(rg.Metadata, "all", 0, backendIndex)
		ri := &eskip.Route{Id: rid}
		if err := applyBackend(ctx, be, ri); err != nil {
			// TODO: log only?
			return nil, err
		}

		if be.Type == serviceBackend {
			if err := applyDefaultFilters(ctx, be.ServiceName, ri); err != nil {
				log.Errorf("Failed to retrieve default filters: %v.", err)
			}
		}

		if ctx.hostRx != "" {
			ri.Predicates = appendPredicate(ri.Predicates, "Host", ctx.hostRx)
		}

		if traffic := ctx.defaultBackendTraffic[beref.BackendName]; traffic < 1 {
			ri.Predicates = appendPredicate(ri.Predicates, "Traffic", traffic)
		}

		storeHostRoute(ctx, ri)
		routes = append(routes, ri)
		routes = appendEastWest(ctx, routes, ri)
		routes = appendHTTPSRedirect(ctx, routes, ri)
	}

	return routes, nil
}

func transformExplicitGroupRoute(ctx *routeContext) (*eskip.Route, error) {
	// TODO: weight

	gr := ctx.groupRoute
	r := &eskip.Route{Id: ctx.id}

	// Path or PathSubtree, prefer Path if we have, becasuse it is more specifc
	if gr.Path != "" {
		r.Predicates = appendPredicate(r.Predicates, "Path", gr.Path)
	} else if gr.PathSubtree != "" {
		r.Predicates = appendPredicate(r.Predicates, "PathSubtree", gr.PathSubtree)
	}

	if gr.PathRegexp != "" {
		r.Predicates = appendPredicate(r.Predicates, "PathRegexp", gr.PathRegexp)
	}

	if ctx.group.hostRx != "" {
		r.Predicates = appendPredicate(r.Predicates, "Host", ctx.group.hostRx)
	}

	if ctx.method != "" {
		r.Predicates = appendPredicate(r.Predicates, "Method", strings.ToUpper(ctx.method))
	}

	for _, pi := range gr.Predicates {
		ppi, err := eskip.ParsePredicates(pi)
		if err != nil {
			return nil, err
		}

		r.Predicates = append(r.Predicates, ppi...)
	}

	var f []*eskip.Filter
	for _, fi := range gr.Filters {
		ffi, err := eskip.ParseFilters(fi)
		if err != nil {
			return nil, err
		}

		f = append(f, ffi...)
	}

	r.Filters = f
	err := applyBackend(ctx.group, ctx.backend, r)
	return r, err
}

// explicitGroupRoutes creates routes for those route groups that have the
// `route` field explicitly defined.
func explicitGroupRoutes(ctx *routeGroupContext) ([]*eskip.Route, error) {
	// TODO: default filters

	var routes []*eskip.Route
	rg := ctx.routeGroup
	for routeIndex, rgr := range rg.Spec.Routes {
		if len(rgr.Methods) == 0 {
			rgr.Methods = []string{""}
		}

		uniqueMethods := make(map[string]struct{})
		for _, m := range rgr.Methods {
			uniqueMethods[m] = struct{}{}
		}

		backendRefs := rg.Spec.DefaultBackends
		backendTraffic := ctx.defaultBackendTraffic
		if len(rgr.Backends) != 0 {
			backendRefs = rgr.Backends
			backendTraffic = calculateTraffic(rgr.Backends)
		}

		// TODO: handling errors. If we consider the route groups independent, then
		// it should be enough to just log them.

		for method := range uniqueMethods {
			for backendIndex, bref := range backendRefs {
				be, ok := ctx.backendsByName[bref.BackendName]
				if !ok {
					return nil, invalidBackendRef(rg, bref.BackendName)
				}

				r, err := transformExplicitGroupRoute(&routeContext{
					group:      ctx,
					groupRoute: rgr,
					id:         crdRouteID(rg.Metadata, method, routeIndex, backendIndex),
					weight:     bref.Weight,
					method:     method,
					backend:    be,
				})
				if err != nil {
					return nil, err
				}

				if traffic := backendTraffic[bref.BackendName]; traffic < 1 {
					r.Predicates = appendPredicate(r.Predicates, "Traffic", traffic)
				}

				storeHostRoute(ctx, r)
				routes = append(routes, r)
				routes = appendEastWest(ctx, routes, r)
				routes = appendHTTPSRedirect(ctx, routes, r)
			}
		}
	}

	return routes, nil
}

func transformRouteGroup(ctx *routeGroupContext) ([]*eskip.Route, error) {
	rg := ctx.routeGroup
	if len(rg.Spec.Backends) == 0 {
		return nil, fmt.Errorf("missing backend for route group: %s", rg.Metadata.Name)
	}

	ctx.defaultBackendTraffic = calculateTraffic(rg.Spec.DefaultBackends)
	if len(rg.Spec.Routes) == 0 {
		return implicitGroupRoutes(ctx)
	}

	return explicitGroupRoutes(ctx)
}

func (r *routeGroups) convert(s *clusterState, df defaultFilters) ([]*eskip.Route, error) {
	var rs []*eskip.Route

	hostRoutes := make(map[string][]*eskip.Route)
	var missingName, missingSpec bool
	for _, rg := range s.routeGroups {
		if rg.Metadata == nil || rg.Metadata.Name == "" {
			missingName = true
			continue
		}

		if rg.Spec == nil {
			missingSpec = true
			continue
		}

		ctx := &routeGroupContext{
			clusterState:         s,
			defaultFilters:       df,
			routeGroup:           rg,
			hostRx:               createHostRx(rg.Spec.Hosts...),
			hostRoutes:           hostRoutes,
			hasEastWestHost:      hasEastWestHost(r.options.KubernetesEastWestDomain, rg.Spec.Hosts),
			eastWestEnabled:      r.options.KubernetesEnableEastWest,
			eastWestDomain:       r.options.KubernetesEastWestDomain,
			provideHTTPSRedirect: r.options.ProvideHTTPSRedirect,
			httpsRedirectCode:    r.options.HTTPSRedirectCode,
			backendsByName:       mapBackends(rg.Spec.Backends),
		}

		ri, err := transformRouteGroup(ctx)
		if err != nil {
			log.Errorf("Error transforming route group %s: %v.", rg.Metadata.Name, err)
			continue
		}

		rs = append(rs, ri...)
	}

	if missingName {
		log.Error("One or more route groups without a name were detected.")
	}

	if missingSpec {
		log.Error("One or more route groups without a spec were detected.")
	}

	catchAll := hostCatchAllRoutes(hostRoutes, func(host string) string {
		// "catchall" won't conflict with any HTTP method
		return rgRouteID("", toSymbol(host), "catchall", 0, 0)
	})

	rs = append(rs, catchAll...)
	return rs, nil
}