package tunnel

import (
	"net"
	"time"

	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	log "github.com/sirupsen/logrus"
)

func tunnelStreamFields(conn net.Conn) log.Fields {
	fields := log.Fields{
		"tunnel_local": conn.LocalAddr().String(), "tunnel_remote": conn.RemoteAddr().String(),
	}
	if stream, ok := conn.(interface{ StreamID() uint32 }); ok {
		fields["stream_id"] = stream.StreamID()
	}
	return fields
}

func logTunnelCopy(logger *log.Entry, forward, reverse string, started time.Time) func(tunnelcore.CopyResult) {
	return func(result tunnelcore.CopyResult) {
		direction := forward
		if result.Direction == tunnelcore.BToA {
			direction = reverse
		}
		entry := logger.WithFields(log.Fields{
			"direction": direction, "bytes": result.Bytes, "duration": time.Since(started),
		})
		if result.Err != nil {
			entry = entry.WithError(result.Err)
		}
		entry.Debug("Tunnel copy finished")
	}
}
