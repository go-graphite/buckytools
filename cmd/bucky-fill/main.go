package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

import "github.com/go-graphite/buckytools"
import "github.com/go-graphite/buckytools/fill"
import whisper "github.com/go-graphite/go-whisper"

var (
	deleteSourceFiles bool
	recursiveMode     bool
	showLog           bool
	printVersion      bool
)

func usage() {
	fmt.Printf("%s [options] <src> <dst>\n", os.Args[0])
	fmt.Printf("Version: %s\n", buckytools.Version)
	fmt.Printf("\tCopies data points from the whisper database <src> to <dst>\n")
	fmt.Printf("\twithout overwriting existing data in <dst>.\n\n")
	flag.PrintDefaults()
}

func main() {
	flag.Usage = usage
	flag.BoolVar(&printVersion, "version", false, "Display version information.")
	flag.BoolVar(&recursiveMode, "r", false, "Recursive mode")
	flag.BoolVar(&deleteSourceFiles, "d", false, "Delete source file after fill")
	flag.BoolVar(&showLog, "l", false, "Show log")
	flag.Parse()
	if printVersion {
		fmt.Printf("Buckytools version: %s\n", buckytools.Version)
		os.Exit(0)
	}
	if flag.NArg() != 2 {
		usage()
		os.Exit(1)
	}
	if err := run(flag.Arg(0), flag.Arg(1)); err != nil {
		fmt.Fprintf(os.Stderr, "An error occured:\n\t%s\n", err)
		os.Exit(2)
	}
}

func run(source, destination string) error {
	var (
		src, dst string
		err      error
		fs, fd   os.FileInfo
	)
	src, err = filepath.Abs(filepath.Clean(source))
	if err != nil {
		return err
	}
	dst, err = filepath.Abs(filepath.Clean(destination))
	if err != nil {
		return err
	}
	if fs, err = os.Stat(src); os.IsNotExist(err) {
		return err
	}
	if _, err = os.Stat(filepath.Dir(dst)); os.IsNotExist(err) {
		return err
	}
	if fs.IsDir() {
		if !recursiveMode {
			return fmt.Errorf("'%s' is directory. Use recursive mode", source)
		}
		if fd, err = os.Stat(dst); err == nil {
			if !fd.IsDir() {
				return fmt.Errorf("cannot overwrite non-directory '%s' with directory '%s'", destination, source)
			}
		}
		return fillDirectory(src, dst)
	}

	if fd, err = os.Stat(dst); os.IsExist(err) {
		if !fd.IsDir() {
			dst = filepath.Join(dst, filepath.Base(src))
		}
	}
	return fillFile(src, dst)
}

func fillFile(sourceFile, destinationFile string) error {
	if _, err := os.Stat(destinationFile); os.IsNotExist(err) {
		if err := copyMetric(sourceFile, destinationFile); err != nil {
			return err
		}
	} else {
		if err := fill.All(sourceFile, destinationFile); err != nil {
			return err
		}
	}
	if deleteSourceFiles {
		// The sidecar goes first and its failure aborts the delete: dropping
		// the metric while the sidecar survives orphans points that only get
		// cleaned up if this exact metric is later recreated through
		// CreateWithOptions.
		if _, err := whisper.RemoveOutOfOrderSidecar(sourceFile); err != nil {
			return err
		}
		return os.Remove(sourceFile)
	}
	return nil
}

func copyMetric(sourceFile, destinationFile string) error {
	// fill can leave a sidecar of its own next to a half-written destination,
	// so unwinding has to take both or the next run merges that debris.
	discard := func(opErr error) error {
		var cleanupErr error
		if err := os.Remove(destinationFile); err != nil && !os.IsNotExist(err) {
			cleanupErr = fmt.Errorf("remove destination: %w", err)
		}
		if _, err := whisper.RemoveOutOfOrderSidecar(destinationFile); err != nil {
			if cleanupErr != nil {
				cleanupErr = fmt.Errorf("%v; remove destination sidecar: %w", cleanupErr, err)
			} else {
				cleanupErr = fmt.Errorf("remove destination sidecar: %w", err)
			}
		}
		if cleanupErr != nil {
			return fmt.Errorf("%w (cleanup failed: %v)", opErr, cleanupErr)
		}
		return opErr
	}

	if err := copyFile(sourceFile, destinationFile); err != nil {
		return discard(err)
	}

	if _, err := os.Stat(whisper.OutOfOrderSidecarPath(sourceFile)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return discard(err)
	}

	// ponytail: the raw copy above already holds every on-time point, so this
	// rewrites the whole file just to fold in a near-empty sidecar.  Swap in a
	// targeted sidecar merge if the cost shows up on large metrics.
	if err := fill.All(sourceFile, destinationFile); err != nil {
		return discard(err)
	}
	return nil
}

func fillDirectory(sourceDir, destinationDir string) error {
	err := filepath.Walk(sourceDir, func(path string, f os.FileInfo, err error) error {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".wsp") {
			return nil
		}
		dstSuffix := strings.TrimPrefix(filepath.Dir(path), sourceDir)
		dstPath := filepath.Join(destinationDir, dstSuffix)
		if _, err := os.Stat(dstPath); os.IsNotExist(err) {
			if err := os.MkdirAll(dstPath, os.ModePerm); err != nil {
				return err
			}
		}
		dstFile := filepath.Join(dstPath, f.Name())
		err = fillFile(path, dstFile)
		if showLog {
			if err != nil {
				fmt.Printf("[FAIL] [%s] [%s] -> [%s]\n", err.Error(), path, dstFile)
			}
			fmt.Printf("[ OK ] [%s] -> [%s]\n", path, dstFile)
		}
		return err
	})
	return err
}

func copyFile(sourceFile, destinationFile string) error {
	var (
		src, dst *os.File
		err      error
	)
	if src, err = os.Open(sourceFile); err != nil {
		return err
	}
	defer src.Close()
	if dst, err = os.Create(destinationFile); err != nil {
		return err
	}
	defer dst.Close()
	if _, err = io.Copy(dst, src); err != nil {
		return err
	}
	return nil
}
