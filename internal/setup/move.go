package setup

import (
	"context"
	"errors"
	"io"
	"os"
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
	svc, service := usesService(p)
	if service {
		if err := svc.stop(io.Discard); err != nil {
			return res, 0, err
		}
	} else if homelabRunning(p) {
		return res, 0, errors.New("the homelab is running (started without the service?); stop `catflap homelab run` first, then run move again")
	}
	res, err = homelab.Move(ctx, p, joinCode, ttl)
	if service {
		// Start again in any case: with the new relay, or unchanged on failure.
		if serr := svc.start(io.Discard); serr != nil && err == nil {
			err = serr
		}
	}
	if err != nil || !service {
		return res, 0, err
	}
	waitHomelabUp(ctx, p, time.Time{}, 20*time.Second)
	var sent struct{ Sent int }
	homelab.Control(p).Do(ctx, "POST", "/invites/telegram", nil, &sent)
	return res, sent.Sent, nil
}
