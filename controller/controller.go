package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/TwinProduction/gatus/config"
	"github.com/TwinProduction/gatus/security"
	"github.com/TwinProduction/gatus/storage"
	"github.com/TwinProduction/gatus/storage/store/common"
	"github.com/TwinProduction/gatus/storage/store/common/paging"
	"github.com/TwinProduction/gocache"
	"github.com/TwinProduction/health"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	cacheTTL = 10 * time.Second
)

var (
	cache = gocache.NewCache().WithMaxSize(100).WithEvictionPolicy(gocache.FirstInFirstOut)

	// server is the http.Server created by Handle.
	// The only reason it exists is for testing purposes.
	server *http.Server
)

// Handle creates the router and starts the server
func Handle(securityConfig *security.Config, webConfig *config.WebConfig, uiConfig *config.UIConfig, enableMetrics bool) {
	var router http.Handler = CreateRouter(config.StaticFolder, securityConfig, uiConfig, enableMetrics)
	if os.Getenv("ENVIRONMENT") == "dev" {
		router = developmentCorsHandler(router)
	}
	server = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", webConfig.Address, webConfig.Port),
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  15 * time.Second,
	}
	log.Println("[controller][Handle] Listening on " + webConfig.SocketAddress())
	if os.Getenv("ROUTER_TEST") == "true" {
		return
	}
	log.Println("[controller][Handle]", server.ListenAndServe())
}

// Shutdown stops the server
func Shutdown() {
	if server != nil {
		_ = server.Shutdown(context.TODO())
		server = nil
	}
}

// CreateRouter creates the router for the http server
func CreateRouter(staticFolder string, securityConfig *security.Config, uiConfig *config.UIConfig, enabledMetrics bool) *mux.Router {
	router := mux.NewRouter()
	if enabledMetrics {
		router.Handle("/metrics", promhttp.Handler()).Methods("GET")
	}
	router.Handle("/health", health.Handler().WithJSON(true)).Methods("GET")
	router.HandleFunc("/favicon.ico", favIconHandler(staticFolder)).Methods("GET")
	// Endpoints
	router.HandleFunc("/api/v1/services/statuses", secureIfNecessary(securityConfig, serviceStatusesHandler)).Methods("GET") // No GzipHandler for this one, because we cache the content as Gzipped already
	router.HandleFunc("/api/v1/services/{key}/statuses", secureIfNecessary(securityConfig, GzipHandlerFunc(serviceStatusHandler))).Methods("GET")
	// TODO: router.HandleFunc("/api/v1/services/{key}/uptimes", secureIfNecessary(securityConfig, GzipHandlerFunc(serviceUptimesHandler))).Methods("GET")
	// TODO: router.HandleFunc("/api/v1/services/{key}/events", secureIfNecessary(securityConfig, GzipHandlerFunc(serviceEventsHandler))).Methods("GET")
	router.HandleFunc("/api/v1/services/{key}/uptimes/{duration}/badge.svg", uptimeBadgeHandler).Methods("GET")
	router.HandleFunc("/api/v1/services/{key}/response-times/{duration}/badge.svg", responseTimeBadgeHandler).Methods("GET")
	router.HandleFunc("/api/v1/services/{key}/response-times/{duration}/chart.svg", responseTimeChartHandler).Methods("GET")
	// SPA
	router.HandleFunc("/services/{service}", spaHandler(staticFolder, uiConfig)).Methods("GET")
	router.HandleFunc("/", spaHandler(staticFolder, uiConfig)).Methods("GET")
	// Everything else falls back on static content
	router.PathPrefix("/").Handler(GzipHandler(http.FileServer(http.Dir(staticFolder))))
	return router
}

func secureIfNecessary(securityConfig *security.Config, handler http.HandlerFunc) http.HandlerFunc {
	if securityConfig != nil && securityConfig.IsValid() {
		return security.Handler(handler, securityConfig)
	}
	return handler
}

// serviceStatusesHandler handles requests to retrieve all service statuses
// Due to the size of the response, this function leverages a cache.
// Must not be wrapped by GzipHandler
func serviceStatusesHandler(writer http.ResponseWriter, r *http.Request) {
	page, pageSize := extractPageAndPageSizeFromRequest(r)
	gzipped := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	var exists bool
	var value interface{}
	if gzipped {
		writer.Header().Set("Content-Encoding", "gzip")
		value, exists = cache.Get(fmt.Sprintf("service-status-%d-%d-gzipped", page, pageSize))
	} else {
		value, exists = cache.Get(fmt.Sprintf("service-status-%d-%d", page, pageSize))
	}
	var data []byte
	if !exists {
		var err error
		buffer := &bytes.Buffer{}
		gzipWriter := gzip.NewWriter(buffer)
		serviceStatuses, err := storage.Get().GetAllServiceStatuses(paging.NewServiceStatusParams().WithResults(page, pageSize))
		if err != nil {
			log.Printf("[controller][serviceStatusesHandler] Failed to retrieve service statuses: %s", err.Error())
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(err.Error()))
			return
		}
		data, err = json.Marshal(serviceStatuses)
		if err != nil {
			log.Printf("[controller][serviceStatusesHandler] Unable to marshal object to JSON: %s", err.Error())
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte("Unable to marshal object to JSON"))
			return
		}
		_, _ = gzipWriter.Write(data)
		_ = gzipWriter.Close()
		gzippedData := buffer.Bytes()
		cache.SetWithTTL(fmt.Sprintf("service-status-%d-%d", page, pageSize), data, cacheTTL)
		cache.SetWithTTL(fmt.Sprintf("service-status-%d-%d-gzipped", page, pageSize), gzippedData, cacheTTL)
		if gzipped {
			data = gzippedData
		}
	} else {
		data = value.([]byte)
	}
	writer.Header().Add("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(data)
}

// serviceStatusHandler retrieves a single ServiceStatus by group name and service name
func serviceStatusHandler(writer http.ResponseWriter, r *http.Request) {
	page, pageSize := extractPageAndPageSizeFromRequest(r)
	vars := mux.Vars(r)
	serviceStatus, err := storage.Get().GetServiceStatusByKey(vars["key"], paging.NewServiceStatusParams().WithResults(page, pageSize).WithEvents(1, common.MaximumNumberOfEvents))
	if err != nil {
		if err == common.ErrServiceNotFound {
			writer.WriteHeader(http.StatusNotFound)
		} else {
			log.Printf("[controller][serviceStatusHandler] Failed to retrieve service status: %s", err.Error())
			writer.WriteHeader(http.StatusInternalServerError)
		}
		_, _ = writer.Write([]byte(err.Error()))
		return
	}
	if serviceStatus == nil {
		log.Printf("[controller][serviceStatusHandler] Service with key=%s not found", vars["key"])
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte("not found"))
		return
	}
	output, err := json.Marshal(serviceStatus)
	if err != nil {
		log.Printf("[controller][serviceStatusHandler] Unable to marshal object to JSON: %s", err.Error())
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte("unable to marshal object to JSON"))
		return
	}
	writer.Header().Add("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(output)
}
