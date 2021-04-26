package main

import (
	"container/heap"
	"flag"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/pcap"
)

const CAPTURE_SIZE = 9000

// startReportingLoop starts a loop that will periodically output statistics
// on the hottest keys, and optionally, errors that occured in parsing.
func startReportingLoop(config Config, hot_keys *HotKeyPool, errors *HotKeyPool) {
	sleep_duration := time.Duration(config.Interval) * time.Second
	time.Sleep(sleep_duration)
	for {
		st := time.Now()
		rotated_keys := hot_keys.Rotate()
		top_keys := rotated_keys.GetTopKeys()
		rotated_errors := errors.Rotate()
		top_errors := rotated_errors.GetTopKeys()

		// Build output
		output := ""
		/* Show keys */
		i := 0
		for {
			if top_keys.Len() == 0 {
				break
			}

			/* Check if we've reached the specified key limit, but only if
			 * the user didn't specify regular expressions to match on. */
			if len(config.Regexps) == 0 && i >= config.NumItemsToReport {
				break
			}

			key := heap.Pop(top_keys)
			output += fmt.Sprintf("mcsauna.keys.%s %s %d\n", key.(*Key).Name, key.(*Key).Command, key.(*Key).Hits)

			i += 1
		}
		/* Show errors */
		if config.ShowErrors {
			for top_errors.Len() > 0 {
				err := heap.Pop(top_errors)
				output += fmt.Sprintf(
					"mcsauna.errors.%s %d\n", err.(*Key).Name, err.(*Key).Hits)
			}
		}

		// Write to stdout
		if !config.Quiet {
			fmt.Print(output)
		}

		// Write to file
		if config.OutputFile != "" {
			err := ioutil.WriteFile(config.OutputFile, []byte(output), 0666)
			if err != nil {
				panic(err)
			}
		}

		elapsed := time.Now().Sub(st)
		time.Sleep(sleep_duration - elapsed)
	}
}

func main() {
	config_file := flag.String("c", "", "config file")
	interval := flag.Int("n", 0, "reporting interval (seconds, default 5)")
	network_interface := flag.String("i", "", "capture interface (default any)")
	port := flag.Int("p", 0, "capture port (default 11211)")
	num_items_to_report := flag.Int("r", 0, "number of items to report (default 20)")
	quiet := flag.Bool("q", false, "suppress stdout output (default false)")
	output_file := flag.String("w", "", "file to write output to")
	show_errors := flag.Bool("e", true, "show errors in parsing as a metric")
	flag.Parse()

	// Parse Config
	var config Config
	var err error
	if *config_file != "" {
		config_data, _ := ioutil.ReadFile(*config_file)
		config, err = NewConfig(config_data)
		if err != nil {
			panic(err)
		}
	} else {
		config, err = NewConfig([]byte{})
	}

	// Parse CLI Args
	if *interval != 0 {
		config.Interval = *interval
	}
	if *network_interface != "" {
		config.Interface = *network_interface
	}
	if *port != 0 {
		config.Port = *port
	}
	if *num_items_to_report != 0 {
		config.NumItemsToReport = *num_items_to_report
	}
	if *quiet != false {
		config.Quiet = *quiet
	}
	if *output_file != "" {
		config.OutputFile = *output_file
	}
	if *show_errors != true {
		config.ShowErrors = *show_errors
	}

	// Build Regexps
	regexp_keys := NewRegexpKeys()
	for _, re := range config.Regexps {
		regexp_key, err := NewRegexpKey(re.Re, re.Name)
		if err != nil {
			panic(err)
		}
		regexp_keys.Add(regexp_key)
	}

	hot_keys := NewHotKeyPool()
	errors := NewHotKeyPool()

	// Setup pcap
	handle, err := pcap.OpenLive(config.Interface, CAPTURE_SIZE, true, pcap.BlockForever)
	if err != nil {
		panic(err)
	}
	filter := fmt.Sprintf("tcp and dst port %d", config.Port)
	err = handle.SetBPFFilter(filter)
	if err != nil {
		panic(err)
	}
	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())

	go startReportingLoop(config, hot_keys, errors)

	// Grab a packet
	var (
		cmd     string
		payload []byte
		keys    []string
		cmd_err int
	)
	for packet := range packetSource.Packets() {
		app_data := packet.ApplicationLayer()
		if app_data == nil {
			continue
		}
		payload = app_data.Payload()

		// Process data
		//prev_payload_len := 0
		for len(payload) > 0 {
			cmd, keys, payload, cmd_err = parseCommand(payload)

			// ... We keep track of the payload length to make sure we don't end
			// ... up in an infinite loop if one of the processors repeatedly
			// ... sends us the same remainder.  This should never happen, but
			// ... if it does, it would be better to move on to the next packet
			// ... rather than spin CPU doing nothing.
			//if len(payload) == prev_payload_len {
			//	break
			//}
			//prev_payload_len = len(payload)

			if cmd_err == ERR_NONE {

				// Raw key
				if len(config.Regexps) == 0 {
					keysItems := make([]HotKeyPoolItem, 0)
					for _, key := range keys {
						keysItems = append(keysItems, HotKeyPoolItem{
							Name:    key,
							Command: cmd,
						})
					}
					hot_keys.Add(keysItems)
				} else {

					// Regex
					matches := []string{}
					match_errors := []string{}
					for _, key := range keys {
						matched_regex, err := regexp_keys.Match(key)
						if err != nil {
							match_errors = append(match_errors, "match_error")

							// The user has requested that we also show keys that
							// weren't matched at all, probably for debugging.
							if config.ShowUnmatched {
								matches = append(matches, key)
							}

						} else {
							matches = append(matches, matched_regex)
						}
					}
					matchesItems := make([]HotKeyPoolItem, 0)
					for _, match := range matches {
						matchesItems = append(matchesItems, HotKeyPoolItem{
							Name:    match,
							Command: cmd,
						})
					}
					hot_keys.Add(matchesItems)
					matchesErrorsItems := make([]HotKeyPoolItem, 0)
					for _, match := range match_errors {
						matchesItems = append(matchesItems, HotKeyPoolItem{
							Name:    match,
							Command: cmd,
						})
					}
					errors.Add(matchesErrorsItems)
				}
			} else {
				errors.Add([]HotKeyPoolItem{
					HotKeyPoolItem{
						Name:    ERR_TO_STAT[cmd_err],
						Command: "",
					},
				})
			}
		}
	}
}
