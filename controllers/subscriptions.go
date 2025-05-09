package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/RedHatInsights/entitlements-api-go/config"
	l "github.com/RedHatInsights/entitlements-api-go/logger"
	"github.com/RedHatInsights/entitlements-api-go/types"
	"github.com/redhatinsights/platform-go-middlewares/identity"

	"github.com/getsentry/sentry-go"
	"github.com/karlseguin/ccache/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

var configOptions = config.GetConfig().Options
var cache = ccache.New(
	ccache.Configure().
		MaxSize(configOptions.GetInt64(config.Keys.SubsCacheMaxSize)).
		ItemsToPrune(configOptions.GetUint32(config.Keys.SubsCacheItemPrune)),
)
var cacheDuration = time.Second * time.Duration(configOptions.GetInt64(config.Keys.SubsCacheDuration))

var bundleInfo []types.Bundle
var subsQueryFeatures string
var subsFailure = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "it_subscriptions_service_failure",
		Help: "Total number of IT subscriptions service failures",
	},
	[]string{"code"},
)
var subsTimeHistogram = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "it_subscriptions_service_time_taken",
	Help:    "Subscriptions latency distributions.",
	Buckets: prometheus.LinearBuckets(0.25, 0.25, 20),
})

type GetServicesParams struct {
	IncludeBundles []string
	ExcludeBundles []string
	TrialActivated bool
}

const (
	IncludeBundlesParamKey string = "include_bundles"
	ExcludeBundlesParamKey string = "exclude_bundles"
	TrialActivatedParamKey string = "trial_activated"
)

type GetFeatureStatusParams struct {
	OrgId          string
	ForceFreshData bool
}

// SetBundleInfo sets the bundle information fetched from the YAML
func SetBundleInfo(yamlFilePath string) error {
	bundlesYaml, err := os.ReadFile(yamlFilePath)

	if err != nil {
		sentry.CaptureException(err)
		return err
	}

	err = yaml.Unmarshal([]byte(bundlesYaml), &bundleInfo)
	if err != nil {
		sentry.CaptureException(err)
		return err
	}

	return nil
}

func setSubscriptionsQueryFeatures() {
	features := strings.Split(configOptions.GetString(config.Keys.Features), ",")

	var skuBasedFeatures []string
	for _, bundle := range bundleInfo {
		if slices.Contains(features, bundle.Name) && bundle.Skus != nil && len(bundle.Skus) > 0 {
			skuBasedFeatures = append(skuBasedFeatures, bundle.Name)
		}
	}

	subsQueryFeatures = "?features=" + strings.Join(skuBasedFeatures, "&features=")
}

// GetFeatureStatus calls the IT subs service features endpoint and returns the entitlements for specified features/bundles
var GetFeatureStatus = func(params GetFeatureStatusParams) types.SubscriptionsResponse {
	orgID := params.OrgId
	item := cache.Get(orgID)
	entitleAll := configOptions.GetString(config.Keys.EntitleAll)

	if item != nil && !item.Expired() && !params.ForceFreshData {
		return types.SubscriptionsResponse{
			StatusCode: 200,
			Data:       item.Value().(types.FeatureStatus),
			CacheHit:   true,
		}
	}

	if entitleAll == "true" {
		return types.SubscriptionsResponse{
			StatusCode: 200,
			Data:       types.FeatureStatus{},
			CacheHit:   false,
		}
	}

	if subsQueryFeatures == "" { // build the static part of our query only once
		setSubscriptionsQueryFeatures()
	}
	req := configOptions.GetString(config.Keys.SubsHost) +
		configOptions.GetString(config.Keys.SubAPIBasePath) + 
		"featureStatus" + subsQueryFeatures + "&accountId=" + orgID

	resp, err := getClient().Get(req)

	if err != nil {
		sentry.CaptureException(err)
		return types.SubscriptionsResponse{
			Error: err,
			Url:   req,
		}
	}

	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return types.SubscriptionsResponse{
			StatusCode: resp.StatusCode,
			Body:       string(body),
			Error:      nil,
			Data:       types.FeatureStatus{},
			CacheHit:   false,
			Url:        req,
		}
	}

	defer resp.Body.Close()

	// Unmarshaling response from Feature service
	body, _ := io.ReadAll(resp.Body)
	var FeatureStatus types.FeatureStatus
	json.Unmarshal(body, &FeatureStatus)

	cache.Set(orgID, FeatureStatus, cacheDuration)

	return types.SubscriptionsResponse{
		StatusCode: resp.StatusCode,
		Data:       FeatureStatus,
		CacheHit:   false,
		Url:        req,
	}
}

func failOnDependencyError(errMsg string, res types.SubscriptionsResponse, w http.ResponseWriter) {
	dependencyError := types.DependencyErrorDetails{
		DependencyFailure: true,
		Service:           "Subscriptions Service",
		Status:            res.StatusCode,
		Endpoint:          configOptions.GetString(config.Keys.SubsHost),
		Message:           errMsg,
	}

	errorResponse := types.DependencyErrorResponse{Error: dependencyError}
	errorResponsejson, _ := json.Marshal(errorResponse)

	subsFailure.WithLabelValues(strconv.Itoa(res.StatusCode)).Inc()
	http.Error(w, string(errorResponsejson), 500)
}

func setBundlePayload(entitle bool, trial bool) types.EntitlementsSection {
	return types.EntitlementsSection{IsEntitled: entitle, IsTrial: trial}
}

// Services the handler for GETs to /api/entitlements/v1/services/
func Services() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		idObj := identity.Get(req.Context()).Identity
		orgId := idObj.Internal.OrgID

		queryParams := GetServicesParams{
			IncludeBundles: filtersFromParams(req, IncludeBundlesParamKey),
			ExcludeBundles: filtersFromParams(req, ExcludeBundlesParamKey),
			TrialActivated: boolFromParams(req, TrialActivatedParamKey),
		}
		subscriptions := GetFeatureStatus(
			GetFeatureStatusParams{
				OrgId:          orgId,
				ForceFreshData: queryParams.TrialActivated,
			},
		)

		accNum := idObj.AccountNumber
		isInternal := idObj.User.Internal
		validEmailMatch, _ := regexp.MatchString(`^.*@redhat.com$`, idObj.User.Email)

		validAccNum := !(accNum == "" || accNum == "-1")
		validOrgId := !(orgId == "" || orgId == "-1")

		include_filter := queryParams.IncludeBundles
		exclude_filter := queryParams.ExcludeBundles

		if subscriptions.Error != nil {
			errMsg := "Unexpected error while talking to Subs Service"
			l.Log.WithFields(logrus.Fields{"error": subscriptions.Error}).Error(errMsg)
			sentry.CaptureException(subscriptions.Error)
			failOnDependencyError(errMsg, subscriptions, w)
			return
		}

		subsTimeTaken := time.Since(start).Seconds()
		l.Log.WithFields(logrus.Fields{
			"subs_call_duration": subsTimeTaken, 
			"cache_hit": subscriptions.CacheHit, 
			"url": subscriptions.Url,
			"org_id": orgId,
		}).Info("subs call complete")
		subsTimeHistogram.Observe(subsTimeTaken)

		if subscriptions.StatusCode != 200 {
			errMsg := "Got back a non 200 status code from Subscriptions Service"
			l.Log.WithFields(logrus.Fields{"code": subscriptions.StatusCode, "body": subscriptions.Body}).Error(errMsg)

			sentry.WithScope(func(scope *sentry.Scope) {
				scope.SetExtra("response_body", subscriptions.Body)
				scope.SetExtra("response_status", subscriptions.StatusCode)
				sentry.CaptureException(errors.New(errMsg))
			})

			failOnDependencyError(errMsg, subscriptions, w)
			return
		}

		entitlementsResponse := make(map[string]types.EntitlementsSection)
		for _, b := range bundleInfo {
			if len(include_filter) > 0 {
				if !slices.Contains(include_filter, b.Name) {
					continue
				}
			} else if len(exclude_filter) > 0 {
				if slices.Contains(exclude_filter, b.Name) {
					continue
				}
			}

			isEntitled := true
			isTrial := false
			entitleAll := configOptions.GetString(config.Keys.EntitleAll)

			if entitleAll == "true" {
				entitlementsResponse[b.Name] = setBundlePayload(isEntitled, isTrial)
				continue
			}

			if len(b.Skus) > 0 {
				isEntitled = false
				for _, f := range subscriptions.Data.Features {
					if f.Name == b.Name {
						isEntitled = f.IsEntitled
						isTrial = f.IsEval
					}
				}
			}

			if b.UseValidAccNum {
				isEntitled = validAccNum && isEntitled
			}

			if b.UseValidOrgId {
				isEntitled = validOrgId && isEntitled
			}

			if b.UseIsInternal {
				isEntitled = validAccNum && isInternal && validEmailMatch
			}
			entitlementsResponse[b.Name] = setBundlePayload(isEntitled, isTrial)
		}

		obj, err := json.Marshal(entitlementsResponse)

		if err != nil {
			l.Log.WithFields(logrus.Fields{"error": err}).Error("Unexpected error while unmarshalling JSON data from Subs Service")
			sentry.CaptureException(err)
			http.Error(w, http.StatusText(500), 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(obj))
	}
}

func filtersFromParams(req *http.Request, filterName string) []string {
	var filter []string
	list := req.URL.Query().Get(filterName)
	if list != "" {
		filter = strings.Split(list, ",")
	}
	return filter
}

func boolFromParams(req *http.Request, paramName string) bool {
	strParam := req.URL.Query().Get(paramName)

	if strParam == "" {
		return false
	}

	param, err := strconv.ParseBool(strParam)

	if err != nil {
		return false
	}

	return param
}
