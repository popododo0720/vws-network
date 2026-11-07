package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/popododo0720/vws-network/internal/network"
)

// importCommand is the subcommand that copies a SQLite file into the
// Kubernetes store before the SQLite store is removed (vws-network#130).
const importCommand = "import-sqlite"

const importUsage = "usage: vws-network " + importCommand +
	" -from FILE -store-namespace NS [-kubeconfig PATH] [-dry-run | -verify]"

// importMain runs the import subcommand and returns the exit code: 2 for bad
// flags, 1 for a failed import or a difference, 0 otherwise.
func importMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(importCommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "sqlite database file of the -db store to import; required")
	ns := fs.String("store-namespace", "", "namespace of the Kubernetes store to import into; required")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path; without it the in-cluster service account")
	dryRun := fs.Bool("dry-run", false, "print the objects the import would create and write nothing")
	verify := fs.Bool("verify", false, "write nothing and exit 1 unless the store holds every record of the file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	mode, err := parseImport(*from, *ns, *dryRun, *verify, fs.NArg())
	if err != nil {
		complain(stderr, "vws-network:", err)
		complain(stderr, importUsage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store := network.StoreConfig{Namespace: *ns, Kubeconfig: *kubeconfig}
	if err := network.ImportSQLite(ctx, *from, store, mode, stdout); err != nil {
		complain(stderr, "vws-network:", err)
		return 1
	}
	return 0
}

func parseImport(from, ns string, dryRun, verify bool, extra int) (network.ImportMode, error) {
	switch {
	case extra > 0:
		return 0, errors.New("unexpected arguments")
	case from == "":
		return 0, errors.New("-from is required")
	case ns == "":
		return 0, errors.New("-store-namespace is required")
	case dryRun && verify:
		return 0, errors.New("-dry-run and -verify cannot both be set")
	case dryRun:
		return network.ImportDryRun, nil
	case verify:
		return network.ImportVerify, nil
	}
	return network.ImportWrite, nil
}

// complain writes one line to stderr. A failed write has nowhere left to go;
// the exit code still reports the failure.
func complain(stderr io.Writer, a ...any) {
	fmt.Fprintln(stderr, a...) //nolint:errcheck // stderr is the last place to report to
}
