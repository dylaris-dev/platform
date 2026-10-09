package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveLaunch(t *testing.T) {
	t.Run("paper jar", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "paper-1.20.1-196.jar"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		lf := resolveLaunch(dir)
		if lf.Mode != launchJar || lf.Jar != "paper-1.20.1-196.jar" {
			t.Fatalf("got %+v", lf)
		}
	})
	t.Run("modern forge argfile", func(t *testing.T) {
		dir := t.TempDir()
		args := filepath.Join(dir, "libraries/net/minecraftforge/forge/1.20.1-47.2.0")
		if err := os.MkdirAll(args, 0755); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(args, "unix_args.txt"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "user_jvm_args.txt"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		lf := resolveLaunch(dir)
		if lf.Mode != launchArgfile {
			t.Fatalf("expected argfile, got %+v", lf)
		}
		if lf.ArgsFile != "libraries/net/minecraftforge/forge/1.20.1-47.2.0/unix_args.txt" {
			t.Fatalf("argsfile: %s", lf.ArgsFile)
		}
		if !lf.UserJvmArgs {
			t.Fatalf("expected user_jvm_args.txt detected")
		}
	})
	t.Run("neoforge argfile", func(t *testing.T) {
		dir := t.TempDir()
		args := filepath.Join(dir, "libraries/net/neoforged/neoforge/20.4.80")
		if err := os.MkdirAll(args, 0755); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(args, "unix_args.txt"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		lf := resolveLaunch(dir)
		if lf.Mode != launchArgfile {
			t.Fatalf("expected argfile, got %+v", lf)
		}
		if lf.ArgsFile != "libraries/net/neoforged/neoforge/20.4.80/unix_args.txt" {
			t.Fatalf("argsfile: %s", lf.ArgsFile)
		}
	})
	t.Run("plain server.jar fallback", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "server.jar"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		lf := resolveLaunch(dir)
		if lf.Mode != launchJar {
			t.Fatalf("expected launchJar, got %+v", lf)
		}
		if lf.Jar != "server.jar" {
			t.Fatalf("jar: %s", lf.Jar)
		}
	})
	t.Run("nothing runnable", func(t *testing.T) {
		lf := resolveLaunch(t.TempDir())
		if lf.Mode != launchNone {
			t.Fatalf("expected none, got %+v", lf)
		}
	})
}

func TestSubServerType(t *testing.T) {
	t.Run("forge", func(t *testing.T) {
		dir := t.TempDir()
		args := filepath.Join(dir, "libraries/net/minecraftforge/forge/1.20.1-47.2.0")
		if err := os.MkdirAll(args, 0755); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(args, "unix_args.txt"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if got := subServerType(dir); got != "forge" {
			t.Fatalf("got %q, want forge", got)
		}
	})
	t.Run("neoforge", func(t *testing.T) {
		dir := t.TempDir()
		args := filepath.Join(dir, "libraries/net/neoforged/neoforge/20.4.80")
		if err := os.MkdirAll(args, 0755); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(args, "unix_args.txt"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if got := subServerType(dir); got != "neoforge" {
			t.Fatalf("got %q, want neoforge", got)
		}
	})
	t.Run("paper", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "paper-1.20.1-196.jar"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if got := subServerType(dir); got != "paper" {
			t.Fatalf("got %q, want paper", got)
		}
	})
	t.Run("fabric", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "fabric-server-launch.jar"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if got := subServerType(dir); got != "fabric" {
			t.Fatalf("got %q, want fabric", got)
		}
	})
	t.Run("purpur", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "purpur-1.20.1-2094.jar"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if got := subServerType(dir); got != "purpur" {
			t.Fatalf("got %q, want purpur", got)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		if got := subServerType(t.TempDir()); got != "unknown" {
			t.Fatalf("got %q, want unknown", got)
		}
	})
}

func TestBuildStartCommand(t *testing.T) {
	t.Run("jar form puts platform Xmx last before -jar", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "paper-1.20.1.jar"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		cmd, err := buildStartCommand(dir, 4096, "-XX:+UseG1GC", "ghcr.io/example/mc-java17:latest")
		if err != nil {
			t.Fatal(err)
		}
		want := "java -Xlog:gc::utctime,level,tags -XX:+UseG1GC -Xms3482M -Xmx3482M -jar paper-1.20.1.jar nogui"
		if cmd != want {
			t.Fatalf("got %q want %q", cmd, want)
		}
	})
	t.Run("argfile form puts platform Xmx after user_jvm_args, before args file", func(t *testing.T) {
		dir := t.TempDir()
		args := filepath.Join(dir, "libraries/net/minecraftforge/forge/1.20.1-47.2.0")
		if err := os.MkdirAll(args, 0755); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(args, "unix_args.txt"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "user_jvm_args.txt"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		cmd, err := buildStartCommand(dir, 6144, "", "ghcr.io/example/mc-java17:latest")
		if err != nil {
			t.Fatal(err)
		}
		want := "java -Xlog:gc::utctime,level,tags @user_jvm_args.txt -Xms5223M -Xmx5223M " +
			"@libraries/net/minecraftforge/forge/1.20.1-47.2.0/unix_args.txt nogui"
		if cmd != want {
			t.Fatalf("got %q want %q", cmd, want)
		}
	})
	t.Run("argfile form: extra flags precede user_jvm_args", func(t *testing.T) {
		dir := t.TempDir()
		args := filepath.Join(dir, "libraries/net/minecraftforge/forge/1.20.1-47.2.0")
		if err := os.MkdirAll(args, 0755); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(args, "unix_args.txt"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "user_jvm_args.txt"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		cmd, err := buildStartCommand(dir, 4096, "-Dfoo=bar", "ghcr.io/example/mc-java17:latest")
		if err != nil {
			t.Fatal(err)
		}
		want := "java -Xlog:gc::utctime,level,tags -Dfoo=bar @user_jvm_args.txt -Xms3482M -Xmx3482M " +
			"@libraries/net/minecraftforge/forge/1.20.1-47.2.0/unix_args.txt nogui"
		if cmd != want {
			t.Fatalf("got %q want %q", cmd, want)
		}
	})
	t.Run("nothing runnable is an error", func(t *testing.T) {
		if _, err := buildStartCommand(t.TempDir(), 1024, "", ""); err == nil {
			t.Fatal("expected error for empty dir")
		}
	})
	t.Run("java8 image skips -Xlog (would refuse to start)", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "paper-1.16.5.jar"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		cmd, err := buildStartCommand(dir, 2048, "", "ghcr.io/example/mc-java8:latest")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(cmd, "-Xlog") {
			t.Fatalf("java8 must not include -Xlog: %s", cmd)
		}
	})
	t.Run("java8 image strips java-9+ flags from extra (ShrinkHeapInSteps would crash)", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "paper-1.16.5.jar"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		// Frontend's default GC flags include -XX:-ShrinkHeapInSteps. On
		// Java 8 the JVM rejects it with "Unrecognized VM option" and
		// fails to start.
		extra := "-XX:+UseG1GC -XX:MaxHeapFreeRatio=40 -XX:MinHeapFreeRatio=15 -XX:-ShrinkHeapInSteps"
		cmd, err := buildStartCommand(dir, 2048, extra, "ghcr.io/example/mc-java8:latest")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(cmd, "ShrinkHeapInSteps") {
			t.Fatalf("java8 must strip ShrinkHeapInSteps: %s", cmd)
		}
		if !strings.Contains(cmd, "-XX:+UseG1GC") || !strings.Contains(cmd, "MaxHeapFreeRatio=40") {
			t.Fatalf("java8 must preserve java-8-safe flags: %s", cmd)
		}
	})
	t.Run("java17 image keeps all flags including ShrinkHeapInSteps", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "paper-1.20.1.jar"), []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		cmd, err := buildStartCommand(dir, 2048, "-XX:-ShrinkHeapInSteps", "ghcr.io/example/mc-java17:latest")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(cmd, "ShrinkHeapInSteps") {
			t.Fatalf("java17 must NOT strip ShrinkHeapInSteps: %s", cmd)
		}
	})
}
