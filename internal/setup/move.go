package setup

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/PaddeK/catflap/internal/homelab"
)

// MoveHomelab moves the homelab to another relay (see homelab.Move): it
// stops the service, switches, starts it again and, if the Telegram bot
// runs, has it post the re-enroll invites. sentToBot is how many it posted.
func MoveHomelab(ctx context.Context, p homelab.Paths, joinCode string, ttl time.Duration, out io.Writer) (res homelab.MoveResult, sentToBot int, err error) {
	if ViaCatflap(os.Getenv("SSH_CONNECTION")) {
		return res, 0, errors.New("you are connected through catflap: moving restarts the homelab and your session would end before you get the new invites.\nRun it on the homelab directly or over your LAN")
	}
	service := os.Getenv("CATFLAP_DIR") == "" && runtime.GOOS == "linux" && hasCmd("systemctl") && unitLoaded(true, homelabService)
	if service {
		if err := run(io.Discard, "systemctl", "--user", "stop", homelabService); err != nil {
			return res, 0, err
		}
	} else if c, err := net.DialTimeout("unix", p.Socket, time.Second); err == nil {
		c.Close()
		return res, 0, errors.New("the homelab is running (started without the service?); stop `catflap homelab run` first, then run move again")
	}
	res, err = homelab.Move(ctx, p, joinCode, ttl)
	if service {
		// Start again in any case: with the new relay, or unchanged on failure.
		if serr := run(io.Discard, "systemctl", "--user", "start", homelabService); serr != nil && err == nil {
			err = serr
		}
	}
	if err != nil || !service {
		return res, 0, err
	}
	var s homelab.StatusInfo
	waitFor(15*time.Second, func() bool {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return homelab.Control(p).Do(cctx, "GET", "/status", nil, &s) == nil
	})
	var sent struct{ Sent int }
	homelab.Control(p).Do(ctx, "POST", "/invites/telegram", nil, &sent)
	return res, sent.Sent, nil
}
