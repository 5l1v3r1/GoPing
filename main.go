package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

const VERSION = "v1.0"

// A JSON formatted file holding config options.
const CONF = "conf.json"

const FAIL_TIME = 1 * time.Minute // Time between tests after a failure.
const SUC_TIME = 5 * time.Minute    // Time between tests after a success.
const MAX_CONSECUTIVE = 3           // Number of failures in a row before alerting.
const MINUTES_BETWEEN_TEXTS = 30    // How long in between alerts if successively failing.

// Twilio is the text messaging service used by GoPing.
// This struct is used to hold config data in order to use
// the Twilio service.
type Twilio struct {
	Url    string
	User   string
	Auth   string
	Number string
}

// In the future, more than just a URL might be used and set in the config.
type Server struct {
	URL       string
}

// Config holds general config values that are sucked in from
// the CONF file. Config also holds a Twilio object for Twilio
// configuration.
type Config struct {
	Servers       []Server
	Numbers       []string
	WebserverPort string
	Twilio        Twilio
}

func main() {
	fmt.Println("Starting GoPing", VERSION)
	c := getConfig()

	// Create 2 channels for each server.
	// First one kills the health check and causes a cleanup.
	count := len(c.Servers)
	kill := make([]chan int, count)
	for i := range kill {
		kill[i] = make(chan int, 1) // Create a buffered channel so the kills are non-blocking.
	}

	// Second oe signals it's done cleaning up.
	quit := make([]chan int, count)
	for i := range quit {
		quit[i] = make(chan int, 1) // Buffered channel so quits are non-blocking.
	}

	for i, server := range c.Servers {
		// Pass a kill and quit chan to each.
		go c.Poll(server, kill[i], quit[i]) // Run concurrently for all servers in CONF.
	}

	// We will now setup a goroutine to wait for SIGTERM (Crtl + C)
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)
	go func() {
		for _ = range sigc {
			// ^C is thrown, let's cleanup.
			for i := range kill {
				kill[i] <- 1 // Send a kill signal to each channel.
				// This will cause each goroutine to clean up and then exit.
			}
			for _ = range merge(quit) {
				// Wait for each goroutine to respond that it has quit.
				fmt.Println("[*] Closed channel")
			}
			os.Exit(0)
		}
	}()

	// Web server used to show that this program is still running. Will be pinged by another script.
	http.HandleFunc("/", handler)
	err := http.ListenAndServe(":"+c.WebserverPort, nil)
	if err != nil {
		panic(err)
	}
}

// getConfig() opens the CONF file and parse it as JSON. It
// is stored into a Config object for further use.
func getConfig() (c *Config) {
	file, err := os.Open(CONF)
	if err != nil {
		panic(err)
	}
	defer file.Close() // Close the file when done.

	// Decode the JSON file and fill up Config object.
	decoder := json.NewDecoder(file)
	c = &Config{}
	err = decoder.Decode(&c)
	if err != nil {
		panic(err)
	}
	return
}

// Stupid simple handler for a pinging script to check against.
// Helps make sure Discovery is still running and didn't panic.
func handler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Hi there, I love %s!", r.URL.Path[1:])
}

// Poll takes a server url, and two chans. The given server is then
// checked every 5 minutes to make sure it's still running.
//
// The kill chan is a shutdown signal that main can send causing
// a cleanup. Poll sends on the quit chan to signal it's done and 
// then returns (quits).
func (c *Config) Poll(server Server, kill, quit chan int) {
	// In all cases that this function ends, signal it has quit.
	defer func() {
		quit <- 1
		close(quit) // Close channel.
	}()

	fmt.Println("Server url: " + server.URL)
	url := server.URL

    t := time.Now()
    fmt.Printf("[+] Checking %s at %s.\n", url, t.Local())

    resp, err := http.Get(url)

    if err != nil {
        fmt.Printf("[*] Error reaching %s\n", url)
        fmt.Printf("[ERROR] %s\n", err.Error())
        return
    } else if resp.StatusCode != 200 {
        fmt.Printf("[ERROR] Response was not 200.\n")
        return
    }

	// If first attempt is successful, continue on a loop.
	pass := true
	interval_count := MINUTES_BETWEEN_TEXTS
	cons_count := 0 // Consecutive count.

	for {
		t = time.Now()
		fmt.Printf("[+] Polling %s at %s.\n", url, t.Local())

        resp, err := http.Get(url)


		if err != nil {
			fmt.Printf("[ERROR] %s\n", err.Error())
			pass = false
        } else if resp.StatusCode != 200 {
			fmt.Printf("[ERROR] Response was not 200.\n")
			pass = false
		} else {
			fmt.Printf("[+] Successfully polled %s\n", url)
			pass = true
			cons_count = MAX_CONSECUTIVE
			interval_count = MINUTES_BETWEEN_TEXTS
		}

        resp.Body.Close()

		// If the test fails or passes.
		if pass == false {
			fmt.Printf("[*] GoPing test failed on %s: %s\n", url, err.Error())

			// Check how many consecutive failures have occurred.
			if cons_count == MAX_CONSECUTIVE {
				// Check to see if we are waiting to send another alert.
				if interval_count == MINUTES_BETWEEN_TEXTS {
					// Send alert to all numbers in CONF.
					for _, number := range c.Numbers {
						err = c.Twilio.Text(number, "GoPing Test failed on "+url)
						if err != nil { // If sending through Twilio fails, panic. Pinging script will notice.
							panic(err)
						}
					}
					// Set the interval to 0 as we *just* sent an alert.
					interval_count = 0
				} else {
					// Hasn't been 30 minutes since last alert, so let's increment.
					interval_count++
				}
			} else {
				// Hasn't been 3 consecutive failures yet, so let's increment.
				cons_count++
			}

			// Time to either wait for a kill signal, or timeout and Poll again.
			select {
			case <-kill:
				return
			case <-time.After(FAIL_TIME): // Sleep before next test (1 minute on failure).
			}
		} else { // If the test succeeded.
			// Reset since we passed the test.
			cons_count = 0
			interval_count = MINUTES_BETWEEN_TEXTS // Reset.

			// Either wait for kill or timeout and Poll again.
			select {
			case <-kill:
                // Run any garbage collection tasks here.
				return
			case <-time.After(SUC_TIME): // Sleep before next test (5 minute on success).
			}
		}
	}
}

// Send a text message to the given phone with the given message.
func (t *Twilio) Text(phone, message string) error {
	// Prep the values to be sent via POST
	values := url.Values{"From": {t.Number}, "To": {phone}, "Body": {message}}
	body := strings.NewReader(values.Encode())

	// Prepare the request.
	req, err := http.NewRequest("POST", t.Url, body)
	if err != nil {
		return (err)
	}

	// Add appropriate headers.
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(t.User, t.Auth)

	// Create a client to do the work and then do it.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() // Clean up after ourselves.

	// Check for success.
	if resp.StatusCode == 201 {
		fmt.Println("[+] Text alert sent to " + phone + ".")
	} else {
		fmt.Println("[*] Text alert was not sent! Response Status was", resp.Status)
	}
	return nil
}

// This function takes a series of channels and merges them into
// a single output channel that will signal and close once all
// input channels have signaled and closed.
func merge(cs []chan int) <-chan int {
	var wg sync.WaitGroup
	out := make(chan int)

	// Start an output goroutine for each input channel in cs.  output
	// copies values from c to out until c is closed, then calls wg.Done.
	output := func(c <-chan int) {
		for n := range c {
			out <- n
		}
		wg.Done()
	}
	wg.Add(len(cs))
	for _, c := range cs {
		go output(c)
	}

	// Start a goroutine to close out once all the output goroutines are
	// done.  This must start after the wg.Add call.
	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}
