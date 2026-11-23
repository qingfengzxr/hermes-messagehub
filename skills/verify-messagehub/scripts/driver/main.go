package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := entry(ctx, os.Args[1:])
	stop()
	// entry and runVerification have returned, including fixture cleanup and report writes.
	os.Exit(code)
}
func entry(ctx context.Context, args []string) int {
	o, err := parseOptions(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-messagehub:", err)
		return 2
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-messagehub: working directory unavailable")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	code, err := runVerification(ctx, root, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-messagehub:", err)
	}
	return code
}
func runVerification(ctx context.Context, root string, o options) (code int, problem error) {
	if err := verifyModule(root); err != nil {
		return 2, err
	}
	if err := os.MkdirAll(o.evidence, 0700); err != nil {
		return 2, errors.New("evidence parent unavailable")
	}
	evidence, err := os.MkdirTemp(o.evidence, "run-")
	if err != nil {
		return 2, errors.New("unique evidence directory unavailable")
	}
	a, err := newApp(ctx, root, evidence)
	if err != nil {
		return 2, err
	}
	a.report.Mode = o.mode
	a.report.RequestedFeature = o.feature
	a.feature = "check"
	defer func() {
		if problem != nil {
			a.report.Outcome = "ISSUES"
			_ = a.assert("driver", "verification stopped", "all selected checks succeed", "failed; raw response and stderr withheld", false)
		}
		// Persist early, then prove samples survived fixture deletion, then persist cleanup facts.
		if err := a.writeReport(); err != nil {
			problem = err
			code = 1
		}
		if err := a.cleanup(); err != nil {
			problem = err
			code = 1
		}
		if a.report.SourceSHA256 != "" {
			hash, err := sourceHash(root)
			if err != nil || hash != a.report.SourceSHA256 {
				a.report.Outcome = "ISSUES"
				problem = errors.New("source changed during verification")
				code = 1
			}
		}
		if err := a.writeReport(); err != nil {
			problem = err
			code = 1
		}
		if _, err := os.Stat(filepath.Join(evidence, "report.json")); err != nil {
			problem = errors.New("report did not survive cleanup")
			code = 1
		}
		fmt.Printf("mode=%s requested_feature=%s outcome=%s evidence=%s\n", o.mode, o.feature, a.report.Outcome, evidence)
	}()
	a.ops, err = checkFeatureMap(root)
	if err != nil {
		return 1, err
	}
	if err := a.assert("source + feature map", "index and source entrances", "one indexed document per feature; every source operation mapped", "validated", true); err != nil {
		return 1, err
	}
	a.report.SourceSHA256, err = sourceHash(root)
	if err != nil {
		return 1, err
	}
	if o.mode == "check" {
		a.report.Outcome = "PASS"
		_ = a.report.finishCoverage(a.ops, false)
		return 0, nil
	}
	if len(a.ops) != 12 {
		return 1, errors.New("expected twelve source operations; review changed contract")
	}
	a.feature = "setup"
	if err := a.prepare(); err != nil {
		a.feature = "diagnostic"
		_ = a.doctor()
		return 1, err
	}
	if o.mode == "doctor" {
		a.feature = "doctor"
		if err := a.doctor(); err != nil {
			_ = a.doctor()
			return 1, err
		}
		a.report.Outcome = "PASS"
		_ = a.report.finishCoverage(a.ops, false)
		return 0, nil
	}
	names := featureNames
	if o.feature != "all" {
		names = []string{o.feature}
	}
	doctor := func() error { return a.doctor() }
	drive := func(name string) error { return a.drive(name) }
	// Set the active feature before its doctor too, so diagnostic steps retain context.
	next := 0
	doctor = func() error {
		if next < len(names) {
			a.feature = names[next]
		}
		return a.doctor()
	}
	drive = func(name string) error {
		err := a.drive(name)
		if err == nil {
			next++
		}
		return err
	}
	if err := executeFeatures(a.report, names, doctor, drive); err != nil {
		return 1, err
	}
	if err := a.report.finishCoverage(a.ops, o.feature == "all"); err != nil {
		return 1, err
	}
	a.report.Outcome = "PASS"
	return 0, nil
}
func executeFeatures(report *Report, names []string, doctor func() error, drive func(string) error) error {
	for _, name := range names {
		if err := doctor(); err != nil {
			report.Features = append(report.Features, FeatureResult{Feature: name, Outcome: "BLOCKED"})
			_ = doctor()
			return err
		}
		err := drive(name)
		outcome := "PASS"
		if err != nil {
			outcome = "ISSUES"
		}
		report.Features = append(report.Features, FeatureResult{Feature: name, Outcome: outcome})
		if err != nil {
			_ = doctor()
			return err
		}
	}
	return nil
}

type scenario struct {
	name  string
	drive func() error
}

func (a *App) scenarios() []scenario {
	return []scenario{{"identity", a.identity}, {"messages", a.messages}, {"inbox", a.inbox}, {"files", a.files}, {"recovery", a.recovery}}
}
func scenarioNames(a *App) []string {
	var names []string
	for _, s := range a.scenarios() {
		names = append(names, s.name)
	}
	return names
}
func (a *App) drive(name string) error {
	a.feature = name
	for _, s := range a.scenarios() {
		if s.name == name {
			return s.drive()
		}
	}
	return errors.New("unknown scenario")
}
