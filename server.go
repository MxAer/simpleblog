package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/lib/pq"
)

//go:embed tmpl/*.html
var tmplFS embed.FS

var funcMap = template.FuncMap{
	"minus": func(a, b int) int { return a - b },
	"plus":  func(a, b int) int { return a + b },
	"slice": func(s string, start, end int) string {
		if start > len(s) {
			return ""
		}
		if end > len(s) {
			end = len(s)
		}
		return s[start:end]
	},
}

var (
	db       *sql.DB
	tmpl     = template.Must(template.New("").Funcs(funcMap).ParseFS(tmplFS, "tmpl/*.html"))
	username = "username"
	password = "password"
)

type DBConfig struct {
	Host, User, Password, DBName string
	Port                         int
}

type Post struct {
	ID     string
	Title  string
	Text   string
	Images []string
	Date   time.Time
}

func main() {
	cfg := mustLoadConfig()
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.DBName)

	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Fatal(err)
	}
	if err := createTables(); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", handleMain)
	http.HandleFunc("/blog/", handleBlog)
	http.HandleFunc("/post/", handlePost)
	http.HandleFunc("/create", handleCreateForm)
	http.HandleFunc("/create/post", handleCreatePost)
	http.HandleFunc("/create/letter", handleCreateLetter)
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir("uploads"))))

	log.Println("Listening :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleMain(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	messages, err := getMessages()
	if err != nil {
		log.Println("Error getting messages:", err)
		messages = []struct {
			Name    string
			Email   string
			Message string
			Date    time.Time
		}{}
	}

	data := map[string]interface{}{
		"Messages": messages,
	}

	tmpl.ExecuteTemplate(w, "index.html", data)
}

func handleBlog(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	perPage := 3
	posts, total, err := getPostsPage(page, perPage)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	last := (total + perPage - 1) / perPage
	data := map[string]interface{}{
		"Posts": posts, "Page": page, "Last": last,
	}
	if err := tmpl.ExecuteTemplate(w, "blog.html", data); err != nil {
		log.Println(err)
	}
}

func handlePost(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := filepath.Base(r.URL.Path)
	p, err := getPost(id)
	if err != nil {
		http.Error(w, "post not found", 404)
		return
	}
	if err := tmpl.ExecuteTemplate(w, "post.html", p); err != nil {
		log.Println(err)
	}
}

func handleCreateForm(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tmpl.ExecuteTemplate(w, "login.html", nil)
}
func handleCreateLetter(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	formName := r.FormValue("name")
	formMail := r.FormValue("mail")
	formMessage := r.FormValue("message")
	if err := insertMessage(formName, formMail, formMessage); err != nil {
		http.Error(w, "Error inserting post: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}
func handleCreatePost(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "Error parsing form: "+err.Error(), http.StatusBadRequest)
		return
	}

	formLogin := r.FormValue("login")
	formPass := r.FormValue("pass")
	if formLogin != username || formPass != password {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	title := r.FormValue("title")
	text := r.FormValue("text")
	files := r.MultipartForm.File["images"]

	imgs, err := saveImages(files)
	if err != nil {
		http.Error(w, "Error saving images: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := insertPost(title, text, imgs); err != nil {
		http.Error(w, "Error inserting post: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func createTables() error {
	_, err := db.Exec(`
        CREATE TABLE IF NOT EXISTS blog_posts(
            id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
            title  VARCHAR(200) NOT NULL,
            body   TEXT         NOT NULL,
            images TEXT[]       NOT NULL,
            date   TIMESTAMP DEFAULT NOW()
        )
    `)
	if err != nil {
		return err
	}

	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS hello_letters(
            id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
            name    VARCHAR(45) NOT NULL,
            email   VARCHAR(45) NOT NULL,
            message TEXT NOT NULL,
            date    TIMESTAMP DEFAULT NOW()
        )
    `)
	return err
}

func insertPost(title, body string, images []string) error {
	_, err := db.Exec(`INSERT INTO blog_posts(title,body,images) VALUES($1,$2,$3)`, title, body, pq.Array(images))
	return err
}
func insertMessage(name string, mail string, message string) error {
	_, err := db.Exec(`INSERT INTO hello_letters(name,email,message) VALUES($1,$2,$3)`, name, mail, message)
	return err
}
func getPost(id string) (Post, error) {
	var p Post
	err := db.QueryRow(`SELECT id::text,title,body,images,date FROM blog_posts WHERE id=$1`, id).
		Scan(&p.ID, &p.Title, &p.Text, pq.Array(&p.Images), &p.Date)
	return p, err
}
func getMessages() ([]struct {
	Name    string
	Email   string
	Message string
	Date    time.Time
}, error) {
	rows, err := db.Query(`SELECT name, email, message, date FROM hello_letters ORDER BY date DESC LIMIT 5`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []struct {
		Name    string
		Email   string
		Message string
		Date    time.Time
	}

	for rows.Next() {
		var msg struct {
			Name    string
			Email   string
			Message string
			Date    time.Time
		}
		if err := rows.Scan(&msg.Name, &msg.Email, &msg.Message, &msg.Date); err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}
func getPostsPage(page, perPage int) ([]Post, int, error) {
	var total int
	db.QueryRow(`SELECT count(*) FROM blog_posts`).Scan(&total)

	rows, err := db.Query(`SELECT id::text,title,body,images,date
	                        FROM blog_posts ORDER BY date DESC LIMIT $1 OFFSET $2`,
		perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var posts []Post
	for rows.Next() {
		var p Post
		if err := rows.Scan(&p.ID, &p.Title, &p.Text, pq.Array(&p.Images), &p.Date); err != nil {
			return nil, 0, err
		}
		posts = append(posts, p)
	}
	return posts, total, rows.Err()
}

func mustLoadConfig() DBConfig {
	f, err := os.Open("db.json")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	var c DBConfig
	if err := json.NewDecoder(f).Decode(&c); err != nil {
		log.Fatal(err)
	}
	return c
}

func saveImages(files []*multipart.FileHeader) ([]string, error) {
	dir := "uploads"
	_ = os.MkdirAll(dir, 0755)
	var paths []string
	for _, fh := range files {
		src, err := fh.Open()
		if err != nil {
			return nil, err
		}
		name := strconv.FormatInt(time.Now().UnixNano(), 10) + filepath.Ext(fh.Filename)
		dst, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			src.Close()
			return nil, err
		}
		_, err = io.Copy(dst, src)
		src.Close()
		dst.Close()
		if err != nil {
			return nil, err
		}
		paths = append(paths, "/uploads/"+name)
	}
	return paths, nil
}
