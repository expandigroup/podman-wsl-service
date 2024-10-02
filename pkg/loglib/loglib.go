package loglib

import (
	"github.com/mattn/go-colorable"
	log "github.com/sirupsen/logrus"
)

func SetUpCliToolLogging() {
	log.SetFormatter(&log.TextFormatter{ForceColors: true})
	log.SetOutput(colorable.NewColorableStdout())
	log.SetReportCaller(false)
}

func SetLogLevel(level string) {
	switch level {
	case "trace":
		log.SetLevel(log.TraceLevel)
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	case "fatal":
		log.SetLevel(log.FatalLevel)
	case "panic":
		log.SetLevel(log.PanicLevel)
	default:
		log.Fatalf("Invalid log level: %s (valid values: trace, debug, info, warn, error, fatal, panic)\n", level)
	}
}
