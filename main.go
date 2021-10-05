package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/cli/safeexec"
	"github.com/spf13/cobra"
)

func rootCmd() *cobra.Command {
	return &cobra.Command{
		Use: "user-status",
	}
}

type setOptions struct {
	Message string
	Limited bool
	Expiry  time.Duration
	Emoji   string
	OrgName string
}

func prompt(em emojiManager, opts *setOptions) error {
	emojiChoices := []string{}
	for _, e := range em.Emojis() {
		emojiChoices = append(emojiChoices, fmt.Sprintf("%s %s %s", string(e.codepoint), e.names, e.desc))
	}
	qs := []*survey.Question{
		{
			Name:     "status",
			Prompt:   &survey.Input{Message: "Status"},
			Validate: survey.Required,
		},
		{
			Name: "emoji",
			Prompt: &survey.Select{
				Message: "Emoji",
				Options: emojiChoices,
				Default: 147,
			},
		},
		{
			Name: "limited",
			Prompt: &survey.Confirm{
				Message: "Indicate limited availability?",
			},
		},
		{
			Name: "expiry",
			Prompt: &survey.Select{
				Message: "Clear status in",
				Options: []string{
					"Never",
					"30m",
					"1h",
					"4h",
					"24h",
					"7d",
				},
			},
		},
	}
	answers := struct {
		Status  string
		Emoji   int
		Limited bool
		Expiry  string
	}{}
	err := survey.Ask(qs, &answers)
	if err != nil {
		return err
	}

	if answers.Expiry == "Never" {
		answers.Expiry = "0s"
	}

	if answers.Expiry == "7d" {
		answers.Expiry = "168h"
	}

	opts.Expiry, _ = time.ParseDuration(answers.Expiry)
	opts.Message = answers.Status
	opts.Emoji = em.Emojis()[answers.Emoji].names[0]
	opts.Limited = answers.Limited

	return nil
}

func setCmd() *cobra.Command {
	opts := setOptions{}
	cmd := &cobra.Command{
		Use:   "set <status>",
		Short: "set your GitHub status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.Message = args[0]
			}

			em := newEmojiManager()
			if opts.Message == "" {
				err := prompt(em, &opts)
				if err != nil {
					return err
				}
			}
			return runSet(opts)
		},
	}
	cmd.Flags().StringVarP(&opts.Emoji, "emoji", "e", "thought_balloon", "Emoji for status")
	cmd.Flags().BoolVarP(&opts.Limited, "limited", "l", false, "Indicate limited availability")
	cmd.Flags().DurationVarP(&opts.Expiry, "expiry", "E", time.Duration(0), "Expire status after this duration")
	cmd.Flags().StringVarP(&opts.OrgName, "org", "o", "", "Limit status visibility to an organization")

	return cmd
}

func runSet(opts setOptions) error {
	em := newEmojiManager()
	// TODO org flag -- punted on this bc i have to resolve an org ID and it didn't feel worth it.
	mutation := `mutation($emoji: String!, $message: String!, $limited: Boolean!, $expiry: DateTime) {
		changeUserStatus(input: {emoji: $emoji, message: $message, limitedAvailability: $limited, expiresAt: $expiry}) {
			status {
				message
				emoji
			}
		}
	}`

	limited := "false"
	if opts.Limited {
		limited = "true"
	}

	expiry := "null"
	if opts.Expiry > time.Duration(0) {
		expiry = time.Now().Add(opts.Expiry).Format("2006-01-02T15:04:05-0700")
	}

	emoji := ""
	if opts.Emoji != "" {
		emoji = fmt.Sprintf(":%s:", opts.Emoji)
	}

	cmdArgs := []string{
		"api", "graphql",
		"-f", fmt.Sprintf("query=%s", mutation),
		"-f", fmt.Sprintf("message=%s", opts.Message),
		"-f", fmt.Sprintf("emoji=%s", emoji),
		"-F", fmt.Sprintf("limited=%s", limited),
		"-F", fmt.Sprintf("expiry=%s", expiry),
	}

	out, stderr, err := gh(cmdArgs...)
	if err != nil {
		if !strings.Contains(stderr.String(), "one of the following scopes: ['user']") {
			return err
		}

		fmt.Println("! Sorry, this extension requires the 'user' scope.")
		answer := false
		err = survey.AskOne(
			&survey.Confirm{
				Message: "Would you like to add the user scope now?",
				Default: true,
			}, &answer)
		if err != nil {
			return fmt.Errorf("could not prompt: %w", err)
		}
		if !answer {
			return nil
		}
		if err = ghWithInput("auth", "refresh", "-s", "user"); err != nil {
			return err
		}
		out, _, err = gh(cmdArgs...)
		if err != nil {
			return err
		}
	}

	type response struct {
		Data struct {
			ChangeUserStatus struct {
				Status status
			}
		}
	}
	var resp response
	err = json.Unmarshal(out.Bytes(), &resp)
	if err != nil {
		return fmt.Errorf("failed to deserialize JSON: %w", err)
	}

	if resp.Data.ChangeUserStatus.Status.Emoji != emoji {
		return errors.New("failed to set status. Perhaps try another emoji")
	}

	msg := fmt.Sprintf("✓ Status set to %s %s", emoji, opts.Message)
	fmt.Println(em.ReplaceAll(msg))

	return nil
}

func clearCmd() *cobra.Command {
	opts := setOptions{Message: "", Limited: false, Emoji: ""}
	return &cobra.Command{
		Use:   "clear",
		Short: "clear your GitHub status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSet(opts)
		},
	}
}

type getOptions struct {
	Login string
}

func getCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get [<username>]",
		Short: "get a GitHub user's status or your own",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := getOptions{}
			if len(args) > 0 {
				opts.Login = args[0]
			}
			return runGet(opts)
		},
	}
}

type status struct {
	IndicatesLimitedAvailability bool
	Message                      string
	Emoji                        string
}

func runGet(opts getOptions) error {
	if strings.Contains(opts.Login, "/") {
		return runGetTeam(opts)
	} else {
		return runGetUser(opts)
	}
}

func runGetTeam(opts getOptions) error {
	arr := strings.Split(opts.Login, "/")
	login, slug := arr[0], arr[1]
	nodes, err := apiTeam(login, slug)
	if err != nil {
		return err
	}

	em := newEmojiManager()
	for _, n := range *nodes {
		availability := ""
		if n.IndicatesLimitedAvailability {
			availability = "(availability is limited)"
		}
		msg := fmt.Sprintf("%s: %s %s %s", n.User.Login, n.Emoji, n.Message, availability)

		fmt.Println(em.ReplaceAll(msg))
	}

	return nil
}

func runGetUser(opts getOptions) error {
	em := newEmojiManager()
	s, err := apiStatus(opts.Login)
	if err != nil {
		return err
	}

	availability := ""
	if s.IndicatesLimitedAvailability {
		availability = "(availability is limited)"
	}
	msg := fmt.Sprintf("%s %s %s", s.Emoji, s.Message, availability)

	fmt.Println(em.ReplaceAll(msg))

	return nil
}

type memberStatus struct {
	IndicatesLimitedAvailability bool
	Message                      string
	Emoji                        string
	User                         struct {
		Login string
	}
}

func apiTeam(login string, slug string) (*[]memberStatus, error) {
	if login == "" {
		login = "{owner}"
	}
	// TODO: supports over 100 members
	query := fmt.Sprintf(
		`query {
      organization(login:"%s") {
        team(slug:"%s") {
          memberStatuses(first: 100) {
            nodes { indicatesLimitedAvailability message emoji user { login } }
          }
        }
      }
    }`, login, slug)

	args := []string{"api", "graphql", "-f", fmt.Sprintf("query=%s", query)}
	sout, _, err := gh(args...)
	if err != nil {
		return nil, err
	}

	type response struct {
		Data struct {
			Organization struct {
				Team struct {
					MemberStatuses struct {
						Nodes []memberStatus
					}
				}
			}
		}
	}
	var resp response
	err = json.Unmarshal(sout.Bytes(), &resp)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize JSON: %w", err)
	}

	return &resp.Data.Organization.Team.MemberStatuses.Nodes, nil
}

func apiStatus(login string) (*status, error) {
	key := "user"
	query := fmt.Sprintf(
		`query { user(login:"%s") { status { indicatesLimitedAvailability message emoji }}}`,
		login)
	if login == "" {
		key = "viewer"
		query = `query {viewer { status { indicatesLimitedAvailability message emoji }}}`
	}

	args := []string{"api", "graphql", "-f", fmt.Sprintf("query=%s", query)}
	sout, _, err := gh(args...)
	if err != nil {
		return nil, err
	}

	resp := map[string]map[string]map[string]status{}

	err = json.Unmarshal(sout.Bytes(), &resp)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize JSON: %w", err)
	}

	s, ok := resp["data"][key]["status"]
	if !ok {
		return nil, errors.New("failed to deserialize JSON")
	}

	return &s, nil
}

func main() {
	rc := rootCmd()
	rc.AddCommand(setCmd())
	rc.AddCommand(clearCmd())
	rc.AddCommand(getCmd())

	if err := rc.Execute(); err != nil {
		// TODO not bothering as long as cobra is also printing error
		//fmt.Println(err)
		os.Exit(1)
	}
}

// gh shells out to gh, returning STDOUT/STDERR and any error
func gh(args ...string) (sout, eout bytes.Buffer, err error) {
	ghBin, err := safeexec.LookPath("gh")
	if err != nil {
		err = fmt.Errorf("could not find gh. Is it installed? error: %w", err)
		return
	}

	cmd := exec.Command(ghBin, args...)
	cmd.Stderr = &eout
	cmd.Stdout = &sout

	err = cmd.Run()
	if err != nil {
		err = fmt.Errorf("failed to run gh. error: %w, stderr: %s", err, eout.String())
		return
	}

	return
}

// gh shells out to gh, connecting IO handles for user input
func ghWithInput(args ...string) error {
	ghBin, err := safeexec.LookPath("gh")
	if err != nil {
		return fmt.Errorf("could not find gh. Is it installed? error: %w", err)
	}

	cmd := exec.Command(ghBin, args...)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin

	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to run gh. error: %w", err)
	}

	return nil
}
