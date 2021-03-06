package main

import (
	"os"
	"strings"

	"github.com/lupinthe14th/escli/pkg/version"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v2"
)

func main() {
	if err := newApp().Run(os.Args); err != nil {
		log.Fatal().Err(err)
	}
}

func newApp() *cli.App {
	debug := false
	app := cli.NewApp()
	app.Name = "escli"
	app.Usage = "elasticsearch service client by golang"
	app.UseShortOptionHandling = true
	app.Version = strings.TrimPrefix(version.Version, "v")
	app.Flags = []cli.Flag{
		&cli.BoolFlag{
			Name:        "debug",
			Usage:       "debug mode",
			Destination: &debug,
		},
		&cli.StringFlag{
			Name:    "address",
			Aliases: []string{"a", "host", "H", "url", "URL"},
			Usage:   "Elasticsearch url",
			EnvVars: []string{"ELASTICSEARCH_URL"},
			Value:   "http://localhost:9200",
		},
		&cli.StringFlag{
			Name:    "username",
			Aliases: []string{"u"},
			Usage:   "Elasticsearch username",
			EnvVars: []string{"ELASTICSEARCH_USERNAME"},
			Value:   "elasticsearch",
		},
		&cli.StringFlag{
			Name:    "password",
			Aliases: []string{"p"},
			Usage:   "Elasticsearch url",
			EnvVars: []string{"ELASTICSEARCH_PASSWORD"},
			Value:   "secret",
		},
	}
	app.Before = func(c *cli.Context) error {
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
		if debug {
			zerolog.SetGlobalLevel(zerolog.DebugLevel)
		}
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
		return nil
	}

	app.Commands = []*cli.Command{
		searchCommand,
		// System
		infoCommand,
		versionCommand,
	}
	return app
}
