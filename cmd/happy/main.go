package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"happy/internal/gateway"
	"happy/internal/webui"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	dir := env("HAPPY_DATA_DIR", "./data")
	if len(os.Args) > 1 && os.Args[1] == "set-password" {
		flags := flag.NewFlagSet("set-password", flag.ContinueOnError)
		file := flags.String("password-file", "", "файл с паролем")
		data := flags.String("data-dir", dir, "каталог данных")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		if *file == "" {
			return errors.New("укажите --password-file")
		}
		b, err := os.ReadFile(*file)
		if err != nil {
			return errors.New("не удалось прочитать файл пароля")
		}
		password := strings.TrimRight(string(b), "\r\n")
		if len(password) < 12 || len(password) > 72 {
			return errors.New("пароль должен содержать от 12 до 72 байт")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
		if err != nil {
			return err
		}
		store, err := gateway.OpenStore(*data)
		if err != nil {
			return err
		}
		defer store.Close()
		if err = store.SetPassword(hash); err != nil {
			return err
		}
		fmt.Println("Пароль администратора сохранён. Перезапустите сервис для отзыва сессий.")
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("happy 0.1.0 / Xray v26.11.9")
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] != "serve" {
		return errors.New("команды: serve, set-password, version")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	store, err := gateway.OpenStore(dir)
	if err != nil {
		return err
	}
	defer store.Close()
	if file := os.Getenv("HAPPY_PASSWORD_FILE"); file != "" {
		// Explicitly configured file is authoritative on each start; never stored in the Nix store.
		b, e := os.ReadFile(file)
		if e != nil {
			return errors.New("не удалось прочитать HAPPY_PASSWORD_FILE")
		}
		password := strings.TrimRight(string(b), "\r\n")
		if len(password) < 12 || len(password) > 72 {
			return errors.New("пароль должен содержать от 12 до 72 байт")
		}
		hash, e := bcrypt.GenerateFromPassword([]byte(password), 12)
		if e != nil {
			return e
		}
		if e = store.SetPassword(hash); e != nil {
			return e
		}
	}
	runtimeDir := filepath.Join(dir, "runtime")
	if err = os.MkdirAll(runtimeDir, 0700); err != nil {
		return err
	}
	xray := gateway.NewXray(env("HAPPY_XRAY", "xray"), runtimeDir)
	defer xray.Stop()
	service, err := gateway.NewService(store, xray)
	if err != nil {
		return err
	}
	handler, err := gateway.Handler(service, webui.Files(), os.Getenv("HAPPY_ORIGIN"), os.Getenv("HAPPY_SECURE_COOKIE") == "true")
	if err != nil {
		return err
	}
	service.Start(ctx)
	server := &http.Server{Addr: env("HAPPY_LISTEN", "127.0.0.1:8080"), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 40 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	go func() {
		<-ctx.Done()
		shutdown, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("Happy слушает %s", server.Addr)
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
