// Copyright 2015 go-swagger maintainers
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package middleware

import (
	"fmt"
	"net/http"
	fpath "path"
	"regexp"
	"strings"

	"github.com/go-openapi/analysis"
	"github.com/go-openapi/errors"
	"github.com/go-openapi/loads"
	"github.com/go-openapi/runtime"
	"github.com/go-openapi/runtime/middleware/denco"
	"github.com/go-openapi/spec"
	"github.com/go-openapi/strfmt"
)

// RouteParam is a object to capture route params in a framework agnostic way.
// implementations of the muxer should use these route params to communicate with the
// swagger framework
type RouteParam struct {
	Name  string
	Value string
}

// RouteParams the collection of route params
type RouteParams []RouteParam

// Get gets the value for the route param for the specified key
func (r RouteParams) Get(name string) string {
	vv, _, _ := r.GetOK(name)
	if len(vv) > 0 {
		return vv[len(vv)-1]
	}
	return ""
}

// GetOK gets the value but also returns booleans to indicate if a key or value
// is present. This aids in validation and satisfies an interface in use there
//
// The returned values are: data, has key, has value
func (r RouteParams) GetOK(name string) ([]string, bool, bool) {
	for _, p := range r {
		if p.Name == name {
			return []string{p.Value}, true, p.Value != ""
		}
	}
	return nil, false, false
}

// NewRouter creates a new context aware router middleware
func NewRouter(ctx *Context, next http.Handler) http.Handler {
	if ctx.router == nil {
		ctx.router = DefaultRouter(ctx.spec, ctx.api)
	}

	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if _, rCtx, ok := ctx.RouteInfo(r); ok {
			next.ServeHTTP(rw, rCtx)
			return
		}

		// Not found, check if it exists in the other methods first
		if others := ctx.AllowedMethods(r); len(others) > 0 {
			ctx.Respond(rw, r, ctx.analyzer.RequiredProduces(), nil, errors.MethodNotAllowed(r.Method, others))
			return
		}

		ctx.Respond(rw, r, ctx.analyzer.RequiredProduces(), nil, errors.NotFound("path %s was not found", r.URL.EscapedPath()))
	})
}

// RoutableAPI represents an interface for things that can serve
// as a provider of implementations for the swagger router
type RoutableAPI interface {
	HandlerFor(string, string) (http.Handler, bool)
	ServeErrorFor(string) func(http.ResponseWriter, *http.Request, error)
	ConsumersFor([]string) map[string]runtime.Consumer
	ProducersFor([]string) map[string]runtime.Producer
	AuthenticatorsFor(map[string]spec.SecurityScheme) map[string]runtime.Authenticator
	Authorizer() runtime.Authorizer
	Formats() strfmt.Registry
	DefaultProduces() string
	DefaultConsumes() string
}

// Router represents a swagger aware router
type Router interface {
	Lookup(method, path string) (*MatchedRoute, bool)
	OtherMethods(method, path string) []string
}

type defaultRouteBuilder struct {
	spec     *loads.Document
	analyzer *analysis.Spec
	api      RoutableAPI
	records  map[string][]denco.Record
}

type defaultRouter struct {
	spec    *loads.Document
	routers map[string]*denco.Router
}

func newDefaultRouteBuilder(spec *loads.Document, api RoutableAPI) *defaultRouteBuilder {
	return &defaultRouteBuilder{
		spec:     spec,
		analyzer: analysis.New(spec.Spec()),
		api:      api,
		records:  make(map[string][]denco.Record),
	}
}

// DefaultRouter creates a default implemenation of the router
func DefaultRouter(spec *loads.Document, api RoutableAPI) Router {
	builder := newDefaultRouteBuilder(spec, api)
	if spec != nil {
		for method, paths := range builder.analyzer.Operations() {
			for path, operation := range paths {
				fp := fpath.Join(spec.BasePath(), path)
				debugLog("adding route %s %s %q", method, fp, operation.ID)
				builder.AddRoute(method, fp, operation)
			}
		}
	}
	return builder.Build()
}

type routeEntry struct {
	PathPattern    string
	BasePath       string
	Operation      *spec.Operation
	Consumes       []string
	Consumers      map[string]runtime.Consumer
	Produces       []string
	Producers      map[string]runtime.Producer
	Parameters     map[string]spec.Parameter
	Handler        http.Handler
	Formats        strfmt.Registry
	Binder         *untypedRequestBinder
	Authenticators map[string]runtime.Authenticator
	Authorizer     runtime.Authorizer
	Scopes         map[string][]string
}

// MatchedRoute represents the route that was matched in this request
type MatchedRoute struct {
	routeEntry
	Params   RouteParams
	Consumer runtime.Consumer
	Producer runtime.Producer
}

func (d *defaultRouter) Lookup(method, path string) (*MatchedRoute, bool) {
	mth := strings.ToUpper(method)
	debugLog("looking up route for %s %s", method, path)
	if Debug {
		if len(d.routers) == 0 {
			debugLog("there are no known routers")
		}
		for meth := range d.routers {
			debugLog("got a router for %s", meth)
		}
	}
	if router, ok := d.routers[mth]; ok {
		if m, rp, ok := router.Lookup(fpath.Clean(path)); ok && m != nil {
			if entry, ok := m.(*routeEntry); ok {
				debugLog("found a route for %s %s with %d parameters", method, path, len(entry.Parameters))
				var params RouteParams
				for _, p := range rp {
					v, err := pathUnescape(p.Value)
					if err != nil {
						debugLog("failed to escape %q: %v", p.Value, err)
						v = p.Value
					}
					// a workaround to handle fragment/composig parameters until they are supported in denco router
					// check if this parameter is a fragment within a path segment
					if xpos := strings.Index(entry.PathPattern, fmt.Sprintf("{%s}", p.Name)) + len(p.Name) + 2;
						xpos < len(entry.PathPattern) && entry.PathPattern[xpos] != '/' {
						// extract fragment parameters
						ep := strings.Split(entry.PathPattern[xpos:], "/")[0]
						pnames, pvalues := decodeCompositParams(p.Name, v, ep, nil, nil)
						for i, pname := range pnames {
							params = append(params, RouteParam{Name: pname, Value: pvalues[i]})
						}
					} else {
						// use the parameter directly
						params = append(params, RouteParam{Name: p.Name, Value: v})
					}
				}
				return &MatchedRoute{routeEntry: *entry, Params: params}, true
			}
		} else {
			debugLog("couldn't find a route by path for %s %s", method, path)
		}
	} else {
		debugLog("couldn't find a route by method for %s %s", method, path)
	}
	return nil, false
}

func (d *defaultRouter) OtherMethods(method, path string) []string {
	mn := strings.ToUpper(method)
	var methods []string
	for k, v := range d.routers {
		if k != mn {
			if _, _, ok := v.Lookup(fpath.Clean(path)); ok {
				methods = append(methods, k)
				continue
			}
		}
	}
	return methods
}

// convert swagger parameters per path segment into a denco parameter as multiple parameters per segment are not supported in denco
var pathConverter = regexp.MustCompile(`{(.+?)}([^/]*)`)

func decodeCompositParams(name string, value string, pattern string, names []string, values []string) ([]string, []string){
	pleft := strings.Index(pattern, "{")
	names = append(names, name)
	if pleft < 0 {
		if strings.HasSuffix(value, pattern) {
			values = append(values, value[:len(value)-len(pattern)])
		} else {
			values = append(values, "")
		}
	} else {
		toskip := pattern[:pleft]
		pright := strings.Index(pattern, "}")
		vright := strings.Index(value, toskip)
		if vright >= 0 {
			values = append(values, value[:vright])
		} else {
			values = append(values, "")
			value = ""
		}
		return decodeCompositParams(pattern[pleft+1:pright], value[vright+len(toskip):], pattern[pright+1:], names, values)
	}
	return names, values
}

func (d *defaultRouteBuilder) AddRoute(method, path string, operation *spec.Operation) {
	mn := strings.ToUpper(method)

	bp := fpath.Clean(d.spec.BasePath())
	if len(bp) > 0 && bp[len(bp)-1] == '/' {
		bp = bp[:len(bp)-1]
	}

	debugLog("operation: %#v", *operation)
	if handler, ok := d.api.HandlerFor(method, strings.TrimPrefix(path, bp)); ok {
		consumes := d.analyzer.ConsumesFor(operation)
		produces := d.analyzer.ProducesFor(operation)
		parameters := d.analyzer.ParamsFor(method, strings.TrimPrefix(path, bp))
		definitions := d.analyzer.SecurityDefinitionsFor(operation)
		requirements := d.analyzer.SecurityRequirementsFor(operation)
		scopes := make(map[string][]string, len(requirements))
		for _, v := range requirements {
			scopes[v.Name] = v.Scopes
		}


		record := denco.NewRecord(pathConverter.ReplaceAllString(path, ":$1"), &routeEntry{
			BasePath:       bp,
			PathPattern:    path,
			Operation:      operation,
			Handler:        handler,
			Consumes:       consumes,
			Produces:       produces,
			Consumers:      d.api.ConsumersFor(normalizeOffers(consumes)),
			Producers:      d.api.ProducersFor(normalizeOffers(produces)),
			Parameters:     parameters,
			Formats:        d.api.Formats(),
			Binder:         newUntypedRequestBinder(parameters, d.spec.Spec(), d.api.Formats()),
			Authenticators: d.api.AuthenticatorsFor(definitions),
			Authorizer:     d.api.Authorizer(),
			Scopes:         scopes,
		})
		d.records[mn] = append(d.records[mn], record)
	}
}

func (d *defaultRouteBuilder) Build() *defaultRouter {
	routers := make(map[string]*denco.Router)
	for method, records := range d.records {
		router := denco.New()
		_ = router.Build(records)
		routers[method] = router
	}
	return &defaultRouter{
		spec:    d.spec,
		routers: routers,
	}
}
