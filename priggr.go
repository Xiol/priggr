package main

import (
	"bytes"
	"fmt"
	"io"
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
	"github.com/satori/go.uuid"
)

// Global? Come at me, bro.
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
	Syntax          string `json:"syntax"`
	Paste           string `json:"paste"`
	Expires         int64  `json:"expires,string"`
	ExpireTimestamp int64  `json:"-"`
	Hits            int64  `json"-"`
}

func realMain(c *cli.Context) {
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

	if pygpath, err := exec.LookPath("pygmentize"); err != nil {
		log.Fatalf("You do not appear to have Pygments installed. Please install it!")
	}
	setupPyg()

	r := gin.Default()
	r.Use(static.Serve("/", static.LocalFile(c.String("assets"), true)))
	r.GET("/p/:pasteid", getPaste)
	r.GET("/raw/:pasteid", getRawPaste)
	r.POST("/p", storePaste)

	log.Warningf("Priggr serving on %s:%d", c.String("bind"), c.Int("port"))
	r.Run(fmt.Sprintf("%s:%d", c.String("bind"), c.Int("port")))
}

func setupPyg() {
	cmd := exec.Command(pygpath, "-L", "lexers")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		log.Fatalf("Error initialising Pygments: %s", err)
	}

	repl := strings.NewReplacer("* ", "", ":\n", "")

	for {
		line, err := out.ReadString("\n")
		if err != nil && err == io.EOF {
			break
		} else if err != nil {
			log.Fatalf("Error initialising Pygments when processing available lexers: %s", err)
		}

		if !strings.Index(line, "*") {
			continue
		}

		langs = append(langs, repl.Replace(line))
	}

	log.Debugf("Pygments init complete, found %d lexers", len(langs))
}

func doHighlight(code, lexer string) string {
	if lexer == "none" || lexer == "" {
		return code
	}

	defaults := []string{"-f", "html", "-O", "linenos=table,style=colorful"}

	var cmd *exec.Cmd
	if lexer == "autodetect" {
		cmd = exec.Command(pygpath, "-g", defaults...)
	} else {
		cmd := exec.Command(pygpath, "-l", lexer, defaults...)
	}
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

func storePaste(c *gin.Context) {
	paste := Paste{}
	err := c.Bind(&paste)
	if err != nil {
		c.JSON(400, gin.H{"message": fmt.Sprintf("Could not marshal POST data: %s", err)})
		return
	}

	found := false
	for i := range langs {
		if langs[i] == paste.Syntax {
			found = true
		}
	}

	if !found {
		paste.Syntax = "none"
	}

	paste.Created = time.Now().Unix()
	paste.PasteID = uuid.NewV4().String()

	if paste.Expires > 0 {
		paste.ExpireTimestamp = time.Now().Add(time.Duration(paste.Expires) * time.Second).Unix()
	} else {
		paste.ExpireTimestamp = 0
	}
	log.Debugf("Paste data: %+v", paste)

	db.Save(&paste)
	c.JSON(200, gin.H{"message": "ok", "id": paste.PasteID})
}

func dbFindPaste(c *gin.Context) (Paste, error) {
	pasteid := c.Param("pasteid")
	if pasteid == "" {
		c.JSON(400, gin.H{"message": "Paste ID not provided"})
		return Paste{}, fmt.Errorf("Paste ID not provided")
	}

	paste := Paste{}

	db.Find(&paste, "paste_id = ?", pasteid)

	if paste.Paste == "" {
		c.JSON(404, gin.H{"message": "Paste not found"})
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

func getRawPaste(c *gin.Context) {
	paste, err := dbFindPaste(c)
	if err != nil {
		return
	}
	c.String(200, paste.Paste)
}

func getPaste(c *gin.Context) {
	paste, err := dbFindPaste(c)
	if err != nil {
		return
	}
	paste.Paste = doHighlight(paste.Paste, paste.Syntax)
	c.JSON(200, paste)
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
