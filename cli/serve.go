package cli

import (
	"github.com/spf13/pflag"

	"github.com/n6k-io/n6kd/server"
)

func registerServeFlags(flags *pflag.FlagSet, opts *server.ServeOptions) {
	flags.IntVar(&opts.MaxConnections, "max-connections", 0,
		"clients served at once, past which the next is refused; 0 for no cap")
	flags.IntVar(&opts.ParallelRequests, "parallel-requests-per-connection", 0,
		"requests one client has running at once, the rest queued; default the extension's")
	flags.DurationVar(&opts.IdleStreamTimeout, "idle-stream-timeout", 0,
		"end a result stream its client has not read for this long; 0 for never")
	flags.DurationVar(&opts.IdleTransactionTimeout, "idle-transaction-timeout", 0,
		"roll back a client's transaction no request has used for this long; 0 for never")
}
