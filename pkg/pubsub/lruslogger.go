package pubsub

import (
	log "github.com/sirupsen/logrus"
)

type LrusLogger struct{}

func (LrusLogger) Noticef(format string, v ...interface{}) {
	log.Infof(format, v)
}

func (LrusLogger) Warnf(format string, v ...interface{}) {
	log.Warnf(format, v)
}
func (LrusLogger) Fatalf(format string, v ...interface{}) {
	log.Fatalf(format, v)
}
func (LrusLogger) Errorf(format string, v ...interface{}) {
	log.Errorf(format, v)
}
func (LrusLogger) Debugf(format string, v ...interface{}) {
	log.Debugf(format, v)
}
func (LrusLogger) Tracef(format string, v ...interface{}) {
	log.Tracef(format, v)
}
