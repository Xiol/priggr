package main

import (
	"bufio"
	"bytes"
	"fmt"
	"html/template"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"time"

	// Loads of dependencies, and what?
	log "github.com/Sirupsen/logrus"
	"github.com/codegangsta/cli"
	"github.com/gin-gonic/contrib/static"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

// Globals? Come at me, bro.
var db gorm.DB
var pygpath string
var langs []string

type LogFormatter struct{}

func (f *LogFormatter) Format(entry *log.Entry) ([]byte, error) {
	return []byte(fmt.Sprintf("%s [%s] %s\n", entry.Time.Format("2006-01-02 15:04:05.000"), entry.Level.String(), entry.Message)), nil
}

type Paste struct {
	ID              int    `json:"-"`
	PasteID         string `json:"paste_id" gorm:"column:paste_id" sql:"unique_index"`
	Created         int64  `json:"created"`
	Syntax          string `json:"syntax" form:"syntax"`
	Paste           string `json:"paste" form:"paste"`
	Expires         int64  `json:"expires,string" form:"expires"`
	ExpireTimestamp int64  `json:"-"`
	Hits            int64  `json:"-"`
}

func realMain(c *cli.Context) {
	rand.Seed(time.Now().UnixNano())

	lvl, err := log.ParseLevel(c.String("loglevel"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not parse log level. Must be one of: debug, info, warning, error, panic, fatal\n")
		os.Exit(1)
	}

	formatter := &LogFormatter{}
	log.SetFormatter(formatter)
	log.SetOutput(os.Stderr)
	log.SetLevel(lvl)

	db, err = gorm.Open("sqlite3", c.String("database"))
	if err != nil {
		log.Fatalf("Could not open database from %s: %s", c.String("database"), err)
	}
	defer db.Close()

	if c.Bool("sqldebug") {
		db.LogMode(true)
	}

	db.AutoMigrate(&Paste{})
	log.Debug("Database init done")

	if pygpath, err = exec.LookPath("pygmentize"); err != nil {
		log.Fatalf("You do not appear to have Pygments installed. Please install it!")
	}
	setupPyg()

	r := gin.Default()
	r.LoadHTMLGlob(c.String("templates") + "/*")
	r.Use(static.Serve("/static", static.LocalFile(c.String("assets"), true)))
	r.GET("/", index)
	r.POST("/", storePaste)
	r.GET("/raw", index)

	log.Warningf("Priggr serving on %s:%d", c.String("bind"), c.Int("port"))
	r.Run(fmt.Sprintf("%s:%d", c.String("bind"), c.Int("port")))
}

func setupPyg() {
	cmd := exec.Command(pygpath, "-L", "lexers")
	out := &bytes.Buffer{}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		log.Fatalf("Error initialising Pygments: %s", err)
	}

	repl := strings.NewReplacer("* ", "", ":", "")

	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		if strings.Index(scanner.Text(), "*") != 0 {
			continue
		}

		lexer := repl.Replace(scanner.Text())
		ml := strings.Split(lexer, ",")
		lexer = strings.TrimSpace(ml[0])

		langs = append(langs, lexer)
	}

	if err := scanner.Err(); err != nil {
		log.Fatalf("Error when scanning available lexers from Pygments: %s", err)
	}

	log.Infof("Pygments init complete, found %d lexers", len(langs))
	log.Debugf("Lexers: %v", langs)
}

func doHighlight(code, lexer string) string {
	if lexer == "none" || lexer == "" {
		lexer = "text"
	}

	defaults := []string{"-f", "html", "-O", "linenos=table,style=colorful,encoding=utf-8"}

	var cmd *exec.Cmd
	var args []string
	if lexer == "autodetect" {
		args = append(args, "-g")
	} else {
		args = append(args, "-l", lexer)
	}
	args = append(args, defaults...)

	log.Debugf("Running Pygments with args: %v", args)
	cmd = exec.Command(pygpath, args...)
	cmd.Stdin = strings.NewReader(code)

	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		log.Error("Failed to run Pygments: %s. Stderr: %s", err, stderr.String())
		return code
	}

	return out.String()
}

func index(c *gin.Context) {
	var paste Paste
	var err error

	pasteid := c.Query("p")
	if pasteid != "" {
		paste, err = dbFindPaste(pasteid)
		if err != nil {
			c.HTML(404, "index.tmpl", gin.H{
				"Languages": langs,
				"ErrorMsg":  err,
			})
			return
		}
	}

	// Yeah, this is bad. No regrets.
	if strings.Contains(c.Request.URL.RequestURI(), "/raw") {
		c.String(200, paste.Paste)
		return
	}

	var hlpaste template.HTML
	if paste.Paste != "" {
		hlpaste = template.HTML(doHighlight(paste.Paste, paste.Syntax))
	}

	c.HTML(200, "index.tmpl", gin.H{
		"Languages": langs,
		"Syntax":    paste.Syntax,
		"Expires":   paste.Expires,
		"ID":        paste.PasteID,
		"Paste":     hlpaste,
		"RawPaste":  paste.Paste,
	})
	return
}

func storePaste(c *gin.Context) {
	paste := Paste{}
	err := c.Bind(&paste)
	if err != nil {
		log.Errorf("Could not bind paste: %s", err)
		c.HTML(400, "index.tmpl", gin.H{
			"Languages": langs,
			"ErrorMsg":  fmt.Sprintf("Could not parse the paste you sent."),
		})
		return
	}

	if paste.Paste == "" {
		c.HTML(400, "index.tmpl", gin.H{
			"Languages": langs,
			"ErrorMsg":  fmt.Sprintf("You seem to be missing something..."),
		})
	}

	found := false
	for i := range langs {
		if langs[i] == paste.Syntax {
			found = true
		}
	}

	if !found && paste.Syntax != "autodetect" {
		paste.Syntax = "text"
	}

	paste.Created = time.Now().Unix()
	paste.PasteID = fmt.Sprintf("%x", (time.Now().UnixNano()/2)&rand.Int63n(999999999))

	if paste.Expires > 0 {
		paste.ExpireTimestamp = time.Now().Add(time.Duration(paste.Expires) * time.Second).Unix()
	} else {
		paste.ExpireTimestamp = 0
	}
	log.Debugf("Paste data: %+v", paste)

	db.Save(&paste)
	c.Redirect(302, "/?p="+paste.PasteID)
}

func dbFindPaste(pasteid string) (Paste, error) {
	if pasteid == "" {
		return Paste{}, fmt.Errorf("Paste ID not provided")
	}

	paste := Paste{}

	db.Find(&paste, "paste_id = ?", pasteid)

	if paste.Paste == "" {
		return Paste{}, fmt.Errorf("Paste not found")
	}

	paste.Hits++
	db.Save(&paste)

	if paste.Hits > 1 {
		if paste.Expires == -2 {
			// Burn after reading. The first hit will be the redirect after
			// the paste is added to the system, so we trigger this on the
			// second hit which should be when the recipient has read it.
			defer func() {
				db.Delete(&paste)
			}()
		}
	}

	return paste, nil
}

func expirePastes() {
	log.Debug("Expire timer fired, deleting expired pastes")
	db.Where("expires > 0 AND expire_timestamp < ?", time.Now().Unix()).Delete(Paste{})
}

func main() {
	app := cli.NewApp()
	app.Name = "Priggr"
	app.Usage = "Go-based Pastebin-alike"
	app.Author = "Dane Elwell"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "loglevel, l",
			Value: "info",
			Usage: "Logging level: debug, info, warning, error, panic, fatal",
		},
		cli.StringFlag{
			Name:  "database, d",
			Value: "/var/lib/prigger/prigger.db",
			Usage: "Path to sqlite3 database",
		},
		cli.BoolFlag{
			Name:  "sqldebug, s",
			Usage: "Enables sql debugging only when loglevel is set to debug",
		},
		cli.StringFlag{
			Name:  "assets, a",
			Value: "./static",
			Usage: "Path to static assets",
		},
		cli.StringFlag{
			Name:  "templates, t",
			Value: "./templates",
			Usage: "Path to templates",
		},
		cli.StringFlag{
			Name:  "bind, b",
			Value: "0.0.0.0",
			Usage: "Bind to this IP address",
		},
		cli.IntFlag{
			Name:  "port, p",
			Value: 8998,
			Usage: "Use this port for HTTP requests",
		},
	}

	app.Action = realMain

	expireTicker := time.NewTicker(time.Second * 60)

	go func() {
		for {
			<-expireTicker.C
			expirePastes()
		}
	}()

	app.Run(os.Args)
}
