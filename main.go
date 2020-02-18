package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"github.com/gorilla/mux"
	"gopkg.in/ldap.v3"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// Global variables assigned through init function from OS environment variables
var (
	bindUsername string
	bindPassword string
	ldapServer   string
	ldapBasedn   string
	hookURL      string
	buildTag     string
)

// Announcement slice to store data about mattermost reply:s
type Announcement struct {
	Success []Data `json:"success"`
	Fail    []Data `json:"fail"`
}

// UpdateSuccess to add successful mattermost requests
func (a *Announcement) UpdateSuccess(data Data) {
	a.Success = append(a.Success, data)
}

// UpdateFail to add failed mattermost requests
func (a *Announcement) UpdateFail(data Data) {
	a.Fail = append(a.Fail, data)

}

// FormData storing incoming post requests
type FormData struct {
	Topic   string `json:"topic"`
	Message string `json:"message"`
}

// Update FormData with json payload from incoming requests
func (msg *FormData) Update(jsonPayload []byte) error {
	if err := json.Unmarshal(jsonPayload, &msg); err != nil {
		return err
	}
	return nil
}

// Validate FormData if a field in FormData struct is empty
func (msg *FormData) Validate(err *CustomError) bool {
	if msg.Topic == "" {
		err.Update("inputValidation", fmt.Errorf("Empty 'topic'"))
		return false
	} else if msg.Message == "" {
		err.Update("inputValidation", fmt.Errorf("Empty 'message'"))
		return false
	}
	return true
}

// Data for mattermost hook reply
type Data struct {
	Channel string `json:"channel"`
	Reply   string `json:"reply"`
}

// Update Data
func (d *Data) Update(channel string, reply string) {
	d.Channel = channel
	d.Reply = reply
}

// CustomError for holding custom error messages
type CustomError struct {
	Error   string `json:"error"`
	Context string `json:"context"`
}

// Update a CustomError
func (e *CustomError) Update(context string, err error) error {
	e.Error = err.Error()
	e.Context = context
	return nil
}

// Protect route with Basic Auth validated with function ldapAuth
func auth(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || !ldapAuth(user, pass) {
			w.Header().Set("WWW-Authenticate", "Basic realm=\"Insert your Active Directory credentials\"")
			http.Error(w, "Unauthorized.", 401)
			return
		}
		fn(w, r)
	}
}

// Well... Ldap auth
func ldapAuth(username string, password string) bool {

	//Search filter for users to validate
	//Guess also could be set in an env variable...
	searchFilter := fmt.Sprintf("(&(objectClass=person)(sAMAccountName=%s)(memberof=CN=Platform Administrators,OU=Role Based Management,DC=ad,DC=example,DC=com))", username)

	log.Println("Validate credentials for user", username, "using filter", searchFilter)

	// Skipping veryify for now, until I investigated how to do that in a good way
	l, err := ldap.DialTLS("tcp", ldapServer, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		log.Println("connectError", ldapServer, err.Error())
		return false
	}
	defer l.Close()

	// First bind with a read only user
	err = l.Bind(bindUsername, bindPassword)
	if err != nil {
		log.Println("BindError", err.Error())
		return false
	}

	// Search for the given username
	searchRequest := ldap.NewSearchRequest(
		ldapBasedn,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		searchFilter,
		[]string{"dn"},
		nil,
	)

	sr, err := l.Search(searchRequest)
	if err != nil {
		log.Println(err.Error())
		return false
	}

	if len(sr.Entries) == 0 {
		log.Printf("User not found")
		return false
	}

	userdn := sr.Entries[0].DN

	// Bind as the user to verify their password
	err = l.Bind(userdn, password)
	if err != nil {
		log.Println(err.Error())
		return false
	}

	log.Println("Successfully validated user ", username)

	return true
}

// Post data to Mattermost
func postIt(url string, channel string, text string) (string, error) {

	requestBody, err := json.Marshal(map[string]string{"text": text, "channel": channel})

	if err != nil {
		return "", err
	}

	//TLS config. Skip verify... for now
	trConf := &http.Transport{
		MaxIdleConnsPerHost: 10,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	timeout := time.Duration(5 * time.Second)
	client := http.Client{
		Timeout:   timeout,
		Transport: trConf,
	}

	request, err := http.NewRequest("POST", url, bytes.NewBuffer(requestBody))
	if err != nil {
		return "", err
	}

	request.Header.Set("Content-type", "application/json")

	resp, err := client.Do(request)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return "", err
	}

	return string(body), nil
}

// Adding markdown formatted string from a slice ( s = slice to concatenate, pre = markdown to prefix, post = markdown to postfix )
func markdownJoin(s []string, pre string, post string) string {

	var flatten string
	for _, slice := range s {
		if slice != "" {
			flatten = flatten + pre + slice + post
		}
	}
	return flatten
}

// dumpRequest returns data about a request
func dumpRequest(r *http.Request) string {
	var request []string
	url := fmt.Sprintf("%v %v %v", r.Method, r.URL, r.Proto)
	request = append(request, url)
	request = append(request, fmt.Sprintf("Host: %v", r.Host))
	for name, headers := range r.Header {
		name = strings.ToLower(name)
		for _, h := range headers {
			request = append(request, fmt.Sprintf("%v: %v", name, h))
		}
	}
	return strings.Join(request, "; ")
}

// Make those announcements
func v1Mattermost(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s", dumpRequest(r))
	//Just for custom json errors
	var apiError = new(CustomError)

	//Get that channel data
	vars := mux.Vars(r)
	hookChannels := strings.Split(vars["channel"], ",")
	//Keep track of announcements
	var announcements = new(Announcement)
	var data Data

	//Channels to post announcements in,
	if payload, err := ioutil.ReadAll(r.Body); err != nil {
		apiError.Update("Read-Payload", err)
		json.NewEncoder(w).Encode(apiError)
	} else {
		var postData = new(FormData)
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")

		if err := postData.Update(payload); err == nil {

			if ok := postData.Validate(apiError); !(ok) {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(apiError)
			} else {

				//Make the message
				var text = fmt.Sprintf("@channel\n## `ANNOUNCEMENT`\n***\n**TOPIC:** **`%s`**\n%s\n***\n*Announcement broadcasted in channels:* %s\n", strings.ToUpper(postData.Topic), postData.Message, markdownJoin(hookChannels, " `", "` "))
				// Post it to MatterMost
				for _, channel := range hookChannels {
					if body, err := postIt(hookURL, channel, text); err != nil {
						//apiError.Update("postItError", err)
						data.Update(channel, err.Error())
						announcements.UpdateFail(data)
					} else if body != "ok" {
						data.Update(channel, body)
						announcements.UpdateFail(data)
					} else {
						log.Println("Successfylly announced to channel " + channel)
						data.Update(channel, body)
						announcements.UpdateSuccess(data)
					}
				}
				json.NewEncoder(w).Encode(announcements)
			}

		} else {
			apiError.Update("ParseJSON", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(apiError)
		}
	}
}

// Get latest buildtag. This is set with environment variable BUILDTAG.
// In our k8s pipeline env variable is set by jenkins when building image
func latestVersionTag(w http.ResponseWriter, r *http.Request) {

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	tag := map[string]string{"build": buildTag}
	json.NewEncoder(w).Encode(tag)

}

// Initiate variables
func init() {
	//Setting some logging stuff
	log.SetFlags(log.Lshortfile)
	log.SetPrefix(time.Now().Format("2006-01-02 15:04:05") + " ")

	// Validate exported variables
	exportedVariables := []string{"HOOKURL", "BINDUSERNAME", "BINDPASSWORD", "LDAPSERVER", "LDAPBASEDN", "BUILDTAG"}
	for _, env := range exportedVariables {
		if _, ok := os.LookupEnv(env); !ok {
			log.Println("Missing env variable", env)
			os.Exit(1)
		}
	}
	// Assign exported variables
	log.Println("Init env variables", exportedVariables)
	buildTag = os.Getenv("BUILDTAG")
	hookURL = os.Getenv("HOOKURL")
	bindUsername = os.Getenv("BINDUSERNAME")
	bindPassword = os.Getenv("BINDPASSWORD")
	ldapServer = os.Getenv("LDAPSERVER")
	ldapBasedn = os.Getenv("LDAPBASEDN")
}

func main() {
	// Using github.com/gorilla/mux, Initially thought I needed that(to make my life easier), perhaps revert to plain net/http later
	myRouter := mux.NewRouter().StrictSlash(true)
	myRouter.HandleFunc("/api/v1/mattermost/{channel}", auth(v1Mattermost)).Methods("POST")
	myRouter.HandleFunc("/api/v1/buildtag", latestVersionTag).Methods("GET")
	myRouter.PathPrefix("/").Handler(http.StripPrefix("/", http.FileServer(http.Dir("web")))).Methods("GET")
	log.Println("All OK, service started")
	//err := http.ListenAndServeTLS(":8080", "server.crt", "server.key", myRouter)
	err := http.ListenAndServe(":8080", myRouter)
	if err != nil {
		log.Fatal(err)
	}
}
