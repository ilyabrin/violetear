// Package violetear - HTTP router
//
// Basic example:
//
//  package main
//
//  import (
//     "fmt"
//     "github.com/nbari/violetear"
//     "log"
//     "net/http"
//  )
//
//  func catchAll(w http.ResponseWriter, r *http.Request) {
//      fmt.Fprintf(w, r.URL.Path[1:])
//  }
//
//  func helloWorld(w http.ResponseWriter, r *http.Request) {
//      fmt.Fprintf(w, r.URL.Path[1:])
//  }
//
//  func handleUUID(w http.ResponseWriter, r *http.Request) {
//      fmt.Fprintf(w, r.URL.Path[1:])
//  }
//
//  func main() {
//      router := violetear.New()
//      router.LogRequests = true
//      router.RequestID = "REQUEST_LOG_ID"
//
//      router.AddRegex(":uuid", `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
//
//      router.HandleFunc("*", catchAll)
//      router.HandleFunc("/hello/", helloWorld, "GET,HEAD")
//      router.HandleFunc("/root/:uuid/item", handleUUID, "POST,PUT")
//
//      log.Fatal(http.ListenAndServe(":8080", router))
//  }
//
package violetear

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// key int is unexported to prevent collisions with context keys defined in
// other packages.
type key int

// ParamsKey used for the context
const (
	ParamsKey     key = 0
	versionHeader     = "application/vnd."
)

// Router struct
type Router struct {
	// dynamicRoutes map of dynamic routes and regular expressions
	dynamicRoutes dynamicSet

	// Routes to be matched
	routes *Trie

	// Logger
	Logger func(*ResponseWriter, *http.Request)

	// LogRequests yes or no
	LogRequests bool

	// NotFoundHandler configurable http.Handler which is called when no matching
	// route is found. If it is not set, http.NotFound is used.
	NotFoundHandler http.Handler

	// NotAllowedHandler configurable http.Handler which is called when method not allowed.
	NotAllowedHandler http.Handler

	// PanicHandler function to handle panics.
	PanicHandler http.HandlerFunc

	// RequestID name of the header to use or create.
	RequestID string

	// Verbose
	Verbose bool
}

// New returns a new initialized router.
func New() *Router {
	return &Router{
		dynamicRoutes: dynamicSet{},
		routes:        &Trie{},
		Logger:        logger,
		Verbose:       true,
	}
}

// Handle registers the handler for the given pattern (path, http.Handler, methods).
func (v *Router) Handle(path string, handler http.Handler, httpMethods ...string) error {
	var version string
	if i := strings.Index(path, "#"); i != -1 {
		version = path[i+1:]
		path = path[:i]
	}
	pathParts := v.splitPath(path)

	// search for dynamic routes
	for _, p := range pathParts {
		if strings.HasPrefix(p, ":") {
			if _, ok := v.dynamicRoutes[p]; !ok {
				return fmt.Errorf("[%s] not found, need to add it using AddRegex(%q, `your regex`)", p, p)
			}
		}
	}

	// if no methods, accept ALL
	methods := "ALL"
	if len(httpMethods) > 0 && len(strings.TrimSpace(httpMethods[0])) > 0 {
		methods = httpMethods[0]
	}

	if v.Verbose {
		log.Printf("Adding path: %s [%s] %s", path, methods, version)
	}

	if err := v.routes.Set(pathParts, handler, methods, version); err != nil {
		return err
	}
	return nil
}

// HandleFunc add a route to the router (path, http.HandlerFunc, methods)
func (v *Router) HandleFunc(path string, handler http.HandlerFunc, httpMethods ...string) error {
	return v.Handle(path, handler, httpMethods...)
}

// AddRegex adds a ":named" regular expression to the dynamicRoutes
func (v *Router) AddRegex(name, regex string) error {
	return v.dynamicRoutes.Set(name, regex)
}

// MethodNotAllowed default handler for 405
func (v *Router) MethodNotAllowed() http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w,
			http.StatusText(http.StatusMethodNotAllowed),
			http.StatusMethodNotAllowed,
		)
	})
}

// checkMethod check if request method is allowed or not
func (v *Router) checkMethod(node *Trie, method string) http.Handler {
	for _, h := range node.Handler {
		if h.Method == "ALL" {
			return h.Handler
		}
		if h.Method == method {
			return h.Handler
		}
	}
	if v.NotAllowedHandler != nil {
		return v.NotAllowedHandler
	}
	return v.MethodNotAllowed()
}

// match recursively find a handler for the request
func (v *Router) match(node *Trie, path []string, leaf bool, params Params, method, version string) (http.Handler, Params) {
	catchall := false
	if len(node.Handler) > 0 && leaf {
		return v.checkMethod(node, method), params
	} else if node.HasRegex {
		for _, n := range node.Node {
			if strings.HasPrefix(n.path, ":") {
				rx := v.dynamicRoutes[n.path]
				if rx.MatchString(path[0]) {
					// add param to context
					params = params.Add(n.path, path[0])
					path[0] = n.path
					node, path, leaf, _ := node.Get(path, version)
					return v.match(node, path, leaf, params, method, version)
				}
			}
		}
		if node.HasCatchall {
			catchall = true
		}
	} else if node.HasCatchall {
		catchall = true
	}
	if catchall {
		for _, n := range node.Node {
			if n.path == "*" {
				// add "*" to context
				params = params.Add("*", path[0])
				return v.checkMethod(n, method), params
			}
		}
	}
	// NotFound
	if v.NotFoundHandler != nil {
		return v.NotFoundHandler, params
	}
	return http.NotFoundHandler(), params
}

// ServeHTTP dispatches the handler registered in the matched path
func (v *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// panic handler
	defer func() {
		if err := recover(); err != nil {
			if v.PanicHandler != nil {
				v.PanicHandler(w, r)
			} else {
				http.Error(w, http.StatusText(500), http.StatusInternalServerError)
			}
		}
	}()

	// Request-ID
	if v.RequestID != "" {
		if rid := r.Header.Get(v.RequestID); rid != "" {
			w.Header().Set(v.RequestID, rid)
		}
	}

	// wrap ResponseWriter
	var ww *ResponseWriter
	if v.LogRequests {
		ww = NewResponseWriter(w, v.RequestID)
	}

	// set version based on the value of "Accept: application/vnd.*"
	version := r.Header.Get("Accept")
	if i := strings.LastIndex(version, versionHeader); i != -1 {
		version = version[len(versionHeader)+i:]
	} else {
		version = ""
	}

	// _ path never empty, defaults to ("/")
	node, path, leaf, _ := v.routes.Get(v.splitPath(r.URL.Path), version)

	// h http.Handler
	h, p := v.match(node, path, leaf, Params{}, r.Method, version)

	// dispatch request
	if v.LogRequests {
		if len(p) == 0 {
			h.ServeHTTP(ww, r)
		} else {
			h.ServeHTTP(ww, r.WithContext(context.WithValue(r.Context(), ParamsKey, p)))
		}
		v.Logger(ww, r)
	} else {
		if len(p) == 0 {
			h.ServeHTTP(w, r)
		} else {
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ParamsKey, p)))
		}
	}
}

// splitPath returns an slice of the path
func (v *Router) splitPath(p string) []string {
	pathParts := strings.FieldsFunc(p, func(c rune) bool {
		return c == '/'
	})
	// root (empty slice)
	if len(pathParts) == 0 {
		return []string{"/"}
	}
	return pathParts
}
