package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/dropbox/changes-client/client"
	"github.com/dropbox/changes-client/engine"
	"github.com/dropbox/changes-client/reporter"
	"github.com/getsentry/raven-go"
)

const (
	Version = "0.0.8"
)

var (
	sentryDsn = ""
)

func main() {
	showVersion := flag.Bool("version", false, "Prints changes-client version")
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		return
	}

	if sentryDsn != "" {
		sentryClient, err := raven.NewClient(sentryDsn, map[string]string{
			"version": Version,
		})
		if err != nil {
			log.Fatal(err)
		}

		defer func() {
			var packet *raven.Packet
			p := recover()
			switch rval := p.(type) {
			case nil:
				return
			case error:
				packet = raven.NewPacket(rval.Error(), raven.NewException(rval, raven.NewStacktrace(2, 3, nil)))
			default:
				rvalStr := fmt.Sprint(rval)
				packet = raven.NewPacket(rvalStr, raven.NewException(errors.New(rvalStr), raven.NewStacktrace(2, 3, nil)))
			}

			_, ch := sentryClient.Capture(packet, map[string]string{})
			<-ch
			panic(p)
		}()

		run()
	} else {
		run()
	}
}

func run() {
	config, err := client.GetConfig()
	if err != nil {
		panic(err)
	}

	r := reporter.NewReporter(http.DefaultTransport, config.Server, config.JobstepID, config.Debug)
	defer r.Shutdown()

	engine.RunBuildPlan(r, config)
}

func init() {
	flag.StringVar(&sentryDsn, "sentry-dsn", "", "Sentry DSN for reporting errors")
}
