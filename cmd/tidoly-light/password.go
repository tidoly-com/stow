package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/tidoly-com/stow/internal/immich"
	"github.com/tidoly-com/stow/internal/install"
	"github.com/tidoly-com/stow/internal/library"
	"github.com/tidoly-com/stow/internal/store"
)

// A server with no browser of its own — Linux, reached over ssh — sets its owner's password again from
// the terminal: "sudo tidoly-stow password" (2026-10-18). The settings are found where the service keeps
// them, so nobody has to know the path.

const serviceUnit = "/etc/systemd/system/tidoly-stow.service"

var configFlag = regexp.MustCompile(`-config\s+(\S+)`)

// serviceConfig is the settings file of the installed service, or of whoever ran sudo, or of this user;
// "" when none is there.
func serviceConfig() string {
	if unit, err := os.ReadFile(serviceUnit); err == nil {
		if m := configFlag.FindSubmatch(unit); m != nil {
			return string(m[1])
		}
	}
	var homes []string
	if name := os.Getenv("SUDO_USER"); name != "" {
		if u, err := user.Lookup(name); err == nil {
			homes = append(homes, u.HomeDir)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		homes = append(homes, home)
	}
	for _, home := range homes {
		if p := filepath.Join(home, ".config", "tidoly-stow", "config.json"); exists(p) {
			return p
		}
	}
	return ""
}

// setOwnerPassword asks for the new password twice, without showing it, and sets it for the owner: the
// first account made, which owns the server. The server may be running; the database takes both.
func setOwnerPassword(configFile, dataDir string) error {
	if configFile != "" {
		cfg, err := install.Load(configFile)
		if err != nil || cfg.Data == "" {
			return fmt.Errorf("설정 파일 %s 에 사진 폴더가 없습니다 — 설치를 마친 뒤에 하세요", configFile)
		}
		dataDir = cfg.Data
	}
	hidden, err := library.Hidden(dataDir)
	if err != nil {
		return err
	}
	db := filepath.Join(hidden, "stow.db")
	if !exists(db) {
		return fmt.Errorf("tidoly stow 를 찾지 못했습니다 (%s) — sudo 로 실행했는지 확인해 주세요", db)
	}
	st, err := store.Open(db)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	users, err := st.Users(ctx)
	if err != nil {
		return err
	}
	var owners []store.User
	for _, u := range users {
		if u.IsAdmin {
			owners = append(owners, u)
		}
	}
	if len(owners) == 0 {
		return errors.New("아직 관리자 계정이 없습니다 — 브라우저에서 첫 계정을 만들어 주세요")
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i].CreatedAt < owners[j].CreatedAt })
	owner := owners[0]
	fmt.Printf("관리자 %s 의 새 비밀번호를 정합니다.\n", owner.Email)
	first, err := askPassword("새 비밀번호 (네 글자 이상): ")
	if err != nil {
		return err
	}
	again, err := askPassword("한 번 더: ")
	if err != nil {
		return err
	}
	if first != again {
		return errors.New("두 번 넣은 비밀번호가 다릅니다 — 다시 해 주세요")
	}
	if len(first) < 4 || len(first) > 200 {
		return errors.New("비밀번호는 네 글자 이상이어야 합니다")
	}
	if err := st.SetPassword(ctx, owner.ID, immich.HashPassword(first)); err != nil {
		return err
	}
	fmt.Println("바꿨습니다. 집 안에서 이 서버의 로그인 주소와 새 비밀번호로 로그인하세요.")
	return nil
}

var stdin = bufio.NewReader(os.Stdin)

// askPassword reads one line without showing it on a terminal; from a pipe, as it comes.
func askPassword(prompt string) (string, error) {
	fmt.Print(prompt)
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Println()
		return string(b), err
	}
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
