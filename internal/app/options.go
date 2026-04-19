package app

import (
	"flag"
	"fmt"
	"time"
)

// Options is the process configuration parsed from CLI flags. The concrete
// runtime (catalog, auth, health) is built from these by NewRuntime.
type Options struct {
	Listen                string
	ConfigPath            string
	GracefulPeriod        time.Duration
	SessionAttemptTimeout time.Duration
	ProxyTimeout          time.Duration
	ReloadOnSIGHUP        bool
	LogFormat             string
	MetricsListen         string
	Version               string
}

// ParseFlags parses Gridlane CLI flags without starting the service. Returns
// the populated Options, a flag for whether -version was requested, and any
// parse error.
func ParseFlags(args []string) (Options, bool, error) {
	opts := Options{
		Listen:                ":4444",
		ConfigPath:            "router.json",
		GracefulPeriod:        15 * time.Second,
		SessionAttemptTimeout: 30 * time.Second,
		ProxyTimeout:          5 * time.Minute,
		ReloadOnSIGHUP:        true,
		LogFormat:             "text",
	}

	fs := flag.NewFlagSet("gridlane", flag.ContinueOnError)
	fs.StringVar(&opts.Listen, "listen", opts.Listen, "HTTP listen address")
	fs.StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "path to router.json")
	fs.DurationVar(&opts.GracefulPeriod, "graceful-period", opts.GracefulPeriod, "graceful shutdown period")
	fs.DurationVar(&opts.SessionAttemptTimeout, "session-attempt-timeout", opts.SessionAttemptTimeout, "upstream session creation timeout")
	fs.DurationVar(&opts.ProxyTimeout, "proxy-timeout", opts.ProxyTimeout, "upstream proxy timeout")
	fs.BoolVar(&opts.ReloadOnSIGHUP, "reload-on-sighup", opts.ReloadOnSIGHUP, "reload config on SIGHUP")
	fs.StringVar(&opts.LogFormat, "log-format", opts.LogFormat, "log format: text or json")
	fs.StringVar(&opts.MetricsListen, "metrics-listen", "", "optional metrics listen address")

	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return Options{}, false, err
	}
	if opts.LogFormat != "text" && opts.LogFormat != "json" {
		return Options{}, false, fmt.Errorf("unsupported log format %q", opts.LogFormat)
	}
	return opts, *showVersion, nil
}
