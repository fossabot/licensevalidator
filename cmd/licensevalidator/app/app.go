package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"regexp"

	"github.com/xakep666/licensevalidator/pkg/athens"
	"github.com/xakep666/licensevalidator/pkg/cache"
	"github.com/xakep666/licensevalidator/pkg/github"
	"github.com/xakep666/licensevalidator/pkg/golang"
	"github.com/xakep666/licensevalidator/pkg/gopkg"
	"github.com/xakep666/licensevalidator/pkg/goproxy"
	"github.com/xakep666/licensevalidator/pkg/observ"
	"github.com/xakep666/licensevalidator/pkg/override"
	"github.com/xakep666/licensevalidator/pkg/spdx"
	"github.com/xakep666/licensevalidator/pkg/validation"

	"github.com/Masterminds/semver/v3"
	gh "github.com/google/go-github/v18/github"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

type App struct {
	logger *zap.Logger
	server *http.Server
}

func NewApp(cfg Config) (*App, error) {
	var logger *zap.Logger
	if cfg.Debug {
		logger, _ = zap.NewDevelopment()
	} else {
		logger, _ = zap.NewProduction()
	}

	logger.Info("Running with config", zap.Reflect("config", cfg))

	translator, err := translator(logger, &cfg)
	if err != nil {
		return nil, fmt.Errorf("translator init failed: %w", err)
	}

	c, err := setupCache(&cfg, cache.Direct{
		LicenseResolver: &validation.ChainedLicenseResolver{
			LicenseResolvers: []validation.LicenseResolver{
				githubClient(logger, &cfg),
				goproxyClient(logger, &cfg),
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("setup cache failed: %w", err)
	}

	validator, err := validator(logger, &cfg, translator, c)
	if err != nil {
		return nil, fmt.Errorf("validator init failed: %w", err)
	}

	logger.Info("Trying to resolve goproxy addresses", zap.String("goproxy", string(cfg.GoProxy.BaseURL)))

	goproxyAddrs, err := goproxyAddrs(&cfg)
	if err != nil {
		return nil, fmt.Errorf("get goproxy addrs failed: %w", err)
	}

	logger.Info("Found forbidden admission request sources", zap.Strings("sources", goproxyAddrs))

	mux := http.NewServeMux()
	mux.HandleFunc("/athens/admission", athens.AdmissionHandler(
		&athens.InternalValidator{Validator: validator},
		goproxyAddrs...,
	))
	addPprofHandlers(&cfg, mux)

	return &App{
		logger: logger,
		server: &http.Server{
			Addr:    cfg.Server.ListenAddr,
			Handler: observ.Middleware(logger)(mux),
			ErrorLog: func() *log.Logger {
				l, _ := zap.NewStdLogAt(logger, zap.ErrorLevel)
				return l
			}(),
		},
	}, nil
}

func (a *App) Run() error {
	a.logger.Info("Serving HTTP Requests", zap.String("listen_addr", a.server.Addr))
	err := a.server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}

func (a *App) Stop(ctx context.Context) error {
	a.logger.Info("Stopping")
	return a.server.Shutdown(ctx)
}

func setupCache(cfg *Config, cacher cache.Cacher) (cache.Cacher, error) {
	if cfg.Cache == nil {
		return cacher, nil
	}

	switch cfg.Cache.Type {
	case CacheTypeMemory:
		return &cache.MemoryCache{
			Backed: cacher,
		}, nil
	default:
		return nil, fmt.Errorf("invalid cache type: %s", cfg.Cache.Type)
	}
}

func githubClient(log *zap.Logger, cfg *Config) *github.Client {
	httpClient := &http.Client{}

	if cfg.Github.AccessToken != "" {
		httpClient = oauth2.NewClient(context.Background(), oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: string(cfg.Github.AccessToken),
		}))
	}

	return github.NewClient(log, github.ClientParams{
		Client:                      gh.NewClient(httpClient),
		FallbackConfidenceThreshold: cfg.Validation.ConfidenceThreshold,
	})
}

func goproxyClient(log *zap.Logger, cfg *Config) *goproxy.Client {
	if cfg.GoProxy.BaseURL == "" {
		cfg.GoProxy.BaseURL = "https://proxy.golang.org"
	}
	return goproxy.NewClient(log, goproxy.ClientParams{
		HTTPClient:          &http.Client{},
		BaseURL:             string(cfg.GoProxy.BaseURL),
		ConfidenceThreshold: cfg.Validation.ConfidenceThreshold,
	})
}

func goproxyAddrs(cfg *Config) ([]string, error) {
	u, err := url.Parse(string(cfg.GoProxy.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("goproxy url parse failed: %w", err)
	}

	ips, err := (&net.Resolver{
		PreferGo: true,
	}).LookupIPAddr(context.Background(), u.Hostname())
	if err != nil {
		return nil, fmt.Errorf("goproxy addresses lookup failed: %w", err)
	}

	var addrs []string
	for _, v := range ips {
		addrs = append(addrs, v.String())
	}

	return append(addrs, u.Hostname(), u.Host), nil
}

func translator(log *zap.Logger, cfg *Config) (*validation.ChainedTranslator, error) {
	var overrides []override.TranslateOverride

	for _, item := range cfg.PathOverrides {
		m, err := regexp.Compile(item.Match)
		if err != nil {
			return nil, fmt.Errorf("invalid match %s: %w", item.Match, err)
		}

		overrides = append(overrides, override.TranslateOverride{
			Match:   m,
			Replace: item.Replace,
		})
	}

	return &validation.ChainedTranslator{
		Translators: []validation.Translator{
			override.NewTranslator(log, overrides),
			golang.Translator{},
			gopkg.Translator{},
		},
	}, nil
}

func validator(log *zap.Logger, cfg *Config, translator validation.Translator, resolver validation.LicenseResolver) (*validation.NotifyingValidator, error) {
	var unknownLicenseAction validation.UnknownLicenseAction

	switch cfg.Validation.UnknownLicenseAction {
	case UnknownLicenseAllow:
		unknownLicenseAction = validation.UnknownLicenseAllow
	case UnknownLicenseWarn:
		// TODO
		// unknownLicenseAction = validation.UnknownLicenseWarn
		return nil, fmt.Errorf("warning about unknown license currently not supported")
	case UnknownLicenseDeny:
		unknownLicenseAction = validation.UnknownLicenseDeny
	default:
		return nil, fmt.Errorf("unexpected unknown license action %s", cfg.Validation.UnknownLicenseAction)
	}

	var (
		ruleSet validation.RuleSet
		err     error
	)

	ruleSet.WhitelistedModules, err = parseModuleMatchers(cfg.Validation.RuleSet.WhitelistedModules)
	if err != nil {
		return nil, fmt.Errorf("whitelisted modules parse failed: %w", err)
	}

	ruleSet.BlacklistedModules, err = parseModuleMatchers(cfg.Validation.RuleSet.BlacklistedModules)
	if err != nil {
		return nil, fmt.Errorf("blacklisted modules parse failed: %w", err)
	}

	ruleSet.AllowedLicenses, err = parseLicenses(cfg.Validation.RuleSet.AllowedLicenses)
	if err != nil {
		return nil, fmt.Errorf("allowed licenses parse failed: %w", err)
	}

	ruleSet.DeniedLicenses, err = parseLicenses(cfg.Validation.RuleSet.DeniedLicenses)
	if err != nil {
		return nil, fmt.Errorf("denied licenses parse failed: %w", err)
	}

	return validation.NewNotifyingValidator(
		log, validation.NotifyingValidatorParams{
			Validator: validation.NewRuleSetValidator(log, validation.RuleSetValidatorParams{
				Translator:      translator,
				LicenseResolver: resolver,
				RuleSet:         ruleSet,
			}),
			UnknownLicenseAction: unknownLicenseAction,
		}), nil
}

func parseModuleMatchers(ms []ModuleMatcher) ([]validation.ModuleMatcher, error) {
	ret := make([]validation.ModuleMatcher, 0, len(ms))
	for _, item := range ms {
		if item.Name == "" {
			return nil, fmt.Errorf("module name matcher can't have empty name")
		}
		name, err := regexp.Compile(item.Name)
		if err != nil {
			return nil, fmt.Errorf("invalid module name matcher regexp %s: %w", item.Name, err)
		}

		var constraint *semver.Constraints
		if item.VersionConstraint != "" {
			constraint, err = semver.NewConstraint(item.VersionConstraint)
			if err != nil {
				return nil, fmt.Errorf("invalid constraint for module %s (%s): %w", item.Name, item.VersionConstraint, err)
			}
		}

		ret = append(ret, validation.ModuleMatcher{
			Name:    name,
			Version: constraint,
		})
	}

	return ret, nil
}

func parseLicenses(ls []License) ([]validation.License, error) {
	ret := make([]validation.License, 0, len(ls))
	for _, item := range ls {
		var license validation.License

		if item.SPDXID != "" {
			lic, ok := spdx.LicenseByID(item.SPDXID)
			if !ok {
				return nil, fmt.Errorf("license %s not found in SPDX", item.SPDXID)
			}

			license.SPDXID = item.SPDXID
			license.Name = lic.Name
		} else {
			license.Name = item.Name
		}

		ret = append(ret, license)
	}

	return ret, nil
}

func addPprofHandlers(cfg *Config, mux *http.ServeMux) {
	if cfg.Server.EnablePprof {
		mux.HandleFunc("/pprof/", pprof.Index)
		mux.HandleFunc("/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/pprof/profile", pprof.Profile)
		mux.HandleFunc("/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/pprof/trace", pprof.Trace)
	}
}
