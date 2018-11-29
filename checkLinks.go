package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const logHTML = `
<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>{{.Title}}</title>
</head>
<body>
    <div class="">
        <h1>ICCG MoM Bad Links Log - {{.Timestamp}}</h1>
        <p>
            Total links found and checked: {{.TotalLinks}}<br>
            Total bad links:
            <font color="{{if gt .TotalBadLinks 0}}red{{end}}">{{.TotalBadLinks}}</font>
            <br>
            Percent bad links: {{ .PercentBadLinks }} %
        </p>
    </div>
    <div class="">
        {{ range .Pways}}
          {{ if gt .BadLinks 0 }}
            <div class="">
              <h2><a href="{{.URL}}">{{.Name}}</a></h2>
              <span>Total Links: {{ len .LinksOut }} Bad Links: {{ .BadLinks }} Modified Links: {{ .ModifiedLinks }}</span><br>
              <br>
              {{ range .LinksOut }}
                {{ if ne .StatusCode 200 }}
                <div style="padding-left:20px">
                  <span><strong>Node {{ .NodeID }} - {{ .NodeTitle }}</strong></span><br>
                  <span><a href="{{ .URL }}">Offending Link</a></span><br>
                  <span>Status: {{.Status}}</span><br>
                  <br>
                </div>
                {{ end }}
              {{ end }}
            </div>
          {{ end }}
        {{ end }}
        </ul>
    </div>
</body>
</html>
`

var baseURL = "http://app.mapofmedicine.com/mom/250/"

var wg sync.WaitGroup

// in mom nodes, all links are in fmt [link text] or <a href="link"> or a href="javascript('link-escaped')" -- this regex should get all those links, leaving link in capture group 1
var reURL = regexp.MustCompile(`(?:\[|'|")((https?)[^'"\s]+)`)

// var tmpl = template.Must(template.ParseFiles("templates/index.html"))

var tmpl = template.Must(template.New("log").Parse(logHTML))

type pway struct {
	Name          string
	URL           string
	LinksOut      []linkOut
	BadLinks      int
	ModifiedLinks int
	StatusCode    int
}

type linkOut struct {
	NodeID     int
	NodeTitle  string
	URL        string
	Status     string
	StatusCode int
	Modified   string
}

func mapLogin() *http.Client {
	cookieJar, err := cookiejar.New(nil)
	if err != nil {
		log.Fatal(err)
	}
	client := &http.Client{
		Jar: cookieJar,
	}
	_, err = client.Get(baseURL)
	if err != nil {
		log.Fatal(err)
	}
	formData := url.Values{
		// "next": {"http://app.mapofmedicine.com/mom/250/index.html"}
		// "hideSmartcard": {""},
		"userId":   {"REMOVED"},
		"password": {"REMOVED"},
		// "Login": {"Log in"},
	}
	_, err = client.PostForm(baseURL+"index.html", formData)
	if err != nil {
		log.Fatal(err)
	}
	return client
}

func getPwayList(s *http.Client) []pway {
	var pp []pway
	// fetch list of local pathways with links (from homepage widget)
	res, err := s.Get(baseURL + "widget_localisedpathways.html")
	if err != nil {
		log.Fatal(err)
	}
	doc, err := goquery.NewDocumentFromResponse(res)
	if err != nil {
		log.Fatal(err)
	}
	// save each pathway to struct and add to list of pways
	doc.Find("a").Each(func(i int, a *goquery.Selection) {
		p := pway{
			Name:     a.Text(),
			URL:      baseURL + a.AttrOr("href", ""),
			BadLinks: 0,
		}
		pp = append(pp, p)
	})
	fmt.Printf("Number of pathways added: %v\n", len(pp))
	return pp
}

func (p *pway) getLinksOut(s *http.Client) {
	// mom cant handle the traffic, so some page reuests get 404 response. try 10 times with 1 seocnd delay each until success
	// fmt.Println("getting links for", p.Name)
	attempts := 0
	var res *http.Response
	var err error
	for attempts < 10 {
		attempts++
		res, err = s.Get(p.URL)
		if err != nil {
			log.Println("Error requesting", p.URL)
		}
		if res.StatusCode == http.StatusOK {
			break
		}
		defer res.Body.Close() // close connection if non 200 status received on attempt
		time.Sleep(50 * time.Millisecond)
	}
	p.StatusCode = res.StatusCode
	// fmt.Printf("\tRes %v after %v attempts getting %v\n", p.StatusCode, attempts, p.Name)
	doc, err := goquery.NewDocumentFromResponse(res)
	if err != nil {
		log.Fatal(err)
	}
	doc.Find("#pwNodeContainer form[name^='form_node_content_']").Not("[name$='local'], [name$='public'], [name$='national']").Each(func(i int, f *goquery.Selection) {
		nodeID := f.Find("input[name='id']").AttrOr("value", "0")
		nodeTitle := f.Find("input[name='quickInfoTitle']").AttrOr("value", "")
		// these two are strings of html, need to be parsed with goquery to find links
		quickInfoHTML := f.Find("textarea[name='quickInfoBody']").Text()
		localInfoMD := f.Find("input[name='adminInfoTxt']").AttrOr("value", "")
		qlString := quickInfoHTML + localInfoMD
		// use regex to find all links begin with [|'|" http|https and end with space'"<
		linksArray := reURL.FindAllStringSubmatch(qlString, -1)
		// fmt.Printf("\tFound %v links in %v\n", len(linksArray), p.Name)
		// capture group 1 is [][1] of each subArray
		for _, subArray := range linksArray {
			linkText := subArray[1]
			new := linkOut{}
			new.NodeTitle = nodeTitle
			if i, e := strconv.Atoi(nodeID); e == nil {
				new.NodeID = i
			} else {
				new.NodeID = 0
			}
			// some links are escaped in text from site (links opened via javascript('link')), so have to unescape first
			linkTextUE, err := url.QueryUnescape(linkText)
			if err != nil {
				log.Printf("Error unescaping %v\n", linkText)
				linkTextUE = linkText
			}
			linkParsed, err := url.Parse(linkTextUE)
			if err != nil {
				log.Println("Error parsing url:", err.Error(), linkTextUE)
				new.URL = linkTextUE
			} else {
				//then have to re-encode query string correctly because unescape above decoded it
				// its like this iot cover both cases together
				linkParsed.RawQuery = linkParsed.Query().Encode()
				new.URL = linkParsed.String()
			}
			p.LinksOut = append(p.LinksOut, new)
		}
	})
	// fmt.Println("got links for", p.Name)
}

func (l *linkOut) check(s *http.Client) {
	var res *http.Response
	var err error
	maxAttempts := 5
	for i := 0; i < maxAttempts; i++ {
		if i == 0 {
			res, err = s.Head(l.URL)
		} else {
			res, err = s.Get(l.URL)
		}
		if err != nil {
			l.StatusCode = 0
			l.Status = "Request failed, Error = " + err.Error()
			// fmt.Println("ERROR", err.Error(), l.URL)
			if strings.Contains(err.Error(), "x509") {
				// fmt.Printf("509 error -> dropping https for %v\n", l.URL)
				l.URL = strings.Replace(l.URL, "https://", "http://", 1)
				l.Modified = "https -> http, " + err.Error()
			}
		} else {
			l.StatusCode = res.StatusCode
			l.Status = res.Status
			res.Body.Close() //close response body to free up file descriptor - was causing tcp lookup no such host without
		}
		if l.StatusCode == http.StatusOK {
			break //leave for loop, no more attempts needed
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (p *pway) checkLinksOut(s *http.Client) {
	// fmt.Println("checking links for", p.Name)
	for i := range p.LinksOut {
		// fmt.Println("checking link", p.Name[:10], i, p.LinksOut[i].URL[:20])
		p.LinksOut[i].check(s)
		if p.LinksOut[i].StatusCode != http.StatusOK {
			p.BadLinks++
		}
		if p.LinksOut[i].Modified != "" {
			p.ModifiedLinks++
		}
	}
	// fmt.Println("finished checking links for", p.Name[:10])
}

func (p *pway) getAndCheckLinks(s *http.Client) {
	defer wg.Done()
	p.getLinksOut(s)
	p.checkLinksOut(s)
}

type model struct {
	Title           string
	TotalLinks      int
	TotalBadLinks   int
	PercentBadLinks int
	Timestamp       string
	Pways           []pway
}

func doMyTemplate(m model) {
	fmt.Println("Building Log HTML...")
	htmlFile, _ := os.Create("LinkCheck" + time.Now().Format("2006-01-02-1504") + ".html")
	defer htmlFile.Close()

	err := tmpl.Execute(htmlFile, m) //merge
	if err != nil {
		log.Fatalf("template execution: %s", err)
	}
	fmt.Println("Log HTML Complete")
}

func buildModel(pp []pway) model {
	var totalLinks, totalBadLinks, totalModLinks int
	m := model{
		Title: "MoM Link Check",
		Pways: pp,
	} //define an instance with required fields
	for i, p := range pp {
		fmt.Printf("%v\tStatus\t%v\tLinks %v\tBad %v \tMod %v - %v\n", i, p.StatusCode, len(p.LinksOut), p.BadLinks, p.ModifiedLinks, p.Name[:10])
		totalLinks = totalLinks + len(p.LinksOut)
		totalBadLinks = totalBadLinks + p.BadLinks
		totalModLinks = totalModLinks + p.ModifiedLinks
	}
	fmt.Printf("Links: %v\t Bad: %v\tMods: %v\n", totalLinks, totalBadLinks, totalModLinks)
	t := time.Now()
	m.Timestamp = t.Format("Mon Jan 2, 2006 at 15:04")
	m.TotalLinks = totalLinks
	m.TotalBadLinks = totalBadLinks
	m.PercentBadLinks = int((float64(totalBadLinks) / float64(totalLinks)) * 100)
	return m
}

func main() {
	var pways []pway
	fmt.Println("establishing MOM session")
	session := mapLogin()
	// get local pway names and links
	fmt.Println("getting pathways")
	pways = getPwayList(session)
	fmt.Println("getting and checking pway links")
	for i := range pways {
		// for i := 0; i < 2; i++ {
		wg.Add(1)
		// fmt.Printf("Starting Pway %v: %v\n", i, pways[i].Name)
		// fmt.Println("getting and checking pway links", i)
		go pways[i].getAndCheckLinks(session)
		// fmt.Printf("Finished Pway %v: %v\n", i, pways[i].Name)
	}
	wg.Wait()
	fmt.Println("building model")
	m := buildModel(pways)
	fmt.Println("merging model and template")
	doMyTemplate(m)
}
