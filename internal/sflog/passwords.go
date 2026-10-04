package sflog

import (
	"bufio"
	"os"
	"strings"
)

func LoadPasswords(value string) ([]string, error) {
	passwords := []string{""}
	value = strings.TrimSpace(value)
	if value == "" {
		return passwords, nil
	}
	f, err := os.Open(value)
	if err != nil {
		if os.IsNotExist(err) {
			return append(passwords, value), nil
		}
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		pw := strings.TrimSpace(sc.Text())
		if pw == "" {
			continue
		}
		passwords = append(passwords, pw)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return passwords, nil
}
