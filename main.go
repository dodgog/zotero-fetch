package main

import (
    "database/sql"
    "flag"
    "fmt"
    "log"
    "os/exec"
    "path/filepath"
    "strings"
    "unicode/utf8"

    _ "github.com/mattn/go-sqlite3"
)

// Config holds application-wide configuration
type Config struct {
    DBPath      string
    StoragePath string
    Version     string
}

// Item represents a Zotero library item with its metadata
type Item struct {
    StableID    string
    Title       string
    Tags        sql.NullString
    Attachments sql.NullString
}

// Repository handles database operations
type Repository struct {
    db  *sql.DB
    cfg Config
}

// NewRepository creates a new Repository instance
func Newjepository(db *sql.DB, cfg Config) *Repository {
    return &Repository{db: db, cfg: cfg}
}

const baseQuery = `
    SELECT 
        i.key,
        idv.value as title,
        GROUP_CONCAT(DISTINCT t.name) as tags,
        GROUP_CONCAT(DISTINCT child.key || ':' || COALESCE(ia.path, '')) as attachments
    FROM items i
    LEFT JOIN itemData id ON i.itemID = id.itemID 
    LEFT JOIN itemDataValues idv ON id.valueID = idv.valueID
    LEFT JOIN itemTypes it ON i.itemTypeID = it.itemTypeID
    LEFT JOIN itemTags itag ON i.itemID = itag.itemID
    LEFT JOIN tags t ON itag.tagID = t.tagID
    LEFT JOIN itemAttachments ia ON (ia.parentItemID = i.itemID OR ia.itemID = i.itemID)
    LEFT JOIN items child ON ia.itemID = child.itemID
    WHERE it.display = 1
        AND id.fieldID = (SELECT fieldID FROM fields WHERE fieldName = 'title')
        AND NOT EXISTS (
            SELECT 1 FROM itemAttachments 
            WHERE itemAttachments.itemID = i.itemID 
            AND itemAttachments.parentItemID IS NOT NULL)`

// GetByStableID retrieves a single item by its stable ID
func (r *Repository) GetByStableID(stableID string) (*Item, error) {
    query := fmt.Sprintf("%s AND i.key = ? GROUP BY i.itemID", baseQuery)
    
    var item Item
    err := r.db.QueryRow(query, stableID).Scan(
        &item.StableID,
        &item.Title,
        &item.Tags,
        &item.Attachments,
    )
    if err != nil {
        return nil, fmt.Errorf("fetching item: %w", err)
    }
    return &item, nil
}

// ListItems retrieves items matching the given filters
func (r *Repository) ListItems(titleFilter, tagFilter string) ([]*Item, error) {
    queryBuilder := strings.Builder{}
    queryBuilder.WriteString(baseQuery)

    var args []interface{}
    if titleFilter != "" || tagFilter != "" {
        conditions := make([]string, 0, 2)
        if titleFilter != "" {
            conditions = append(conditions, "idv.value LIKE ?")
            args = append(args, "%"+titleFilter+"%")
        }
        if tagFilter != "" {
            conditions = append(conditions, "t.name LIKE ?")
            args = append(args, "%"+tagFilter+"%")
        }
        if len(conditions) > 0 {
            queryBuilder.WriteString(" AND " + strings.Join(conditions, " AND "))
        }
    }

    queryBuilder.WriteString(" GROUP BY i.itemID")

    rows, err := r.db.Query(queryBuilder.String(), args...)
    if err != nil {
        return nil, fmt.Errorf("executing query: %w", err)
    }
    defer rows.Close()

    var items []*Item
    for rows.Next() {
        var item Item
        if err := rows.Scan(
            &item.StableID,
            &item.Title,
            &item.Tags,
            &item.Attachments,
        ); err != nil {
            return nil, fmt.Errorf("scanning row: %w", err)
        }
        items = append(items, &item)
    }

    if err = rows.Err(); err != nil {
        return nil, fmt.Errorf("iterating rows: %w", err)
    }

    return items, nil
}

// CLI handles command-line operations
type CLI struct {
    repo *Repository
    cfg  Config
}

// NewCLI creates a new CLI instance
func NewCLI(repo *Repository, cfg Config) *CLI {
    return &CLI{repo: repo, cfg: cfg}
}

// getStoragePath returns the full storage path for an item's attachment
func (c *CLI) getStoragePath(item *Item) string {
    if !item.Attachments.Valid || item.Attachments.String == "" {
        return ""
    }

    attachments := strings.Split(item.Attachments.String, ",")
    if len(attachments) == 0 {
        return ""
    }

    parts := strings.SplitN(attachments[0], ":", 2)
    if len(parts) != 2 {
        return ""
    }

    return filepath.Join(
        c.cfg.StoragePath,
        parts[0],
        strings.TrimPrefix(parts[1], "storage:"),
    )
}

func truncateString(s string, n int) string {
    if utf8.RuneCountInString(s) <= n {
        return s
    }
    runes := []rune(s)
    return string(runes[:n-3]) + "..."
}

// printItem formats and prints item information
func (c *CLI) printItem(item *Item, verbose bool) {
    if !verbose {
        fmt.Println(item.StableID)
        return
    }

    title := truncateString(item.Title, 25)
    tags := ""
    if item.Tags.Valid {
        tags = truncateString(item.Tags.String, 15)
    }

    if item.Attachments.Valid && item.Attachments.String != "" {
        attachments := strings.Split(item.Attachments.String, ",")
        for _, att := range attachments {
            parts := strings.SplitN(att, ":", 2)
            if len(parts) == 2 {
                storagePath := filepath.Join(
                    c.cfg.StoragePath,
                    parts[0],
                    strings.TrimPrefix(parts[1], "storage:"),
                )
                fmt.Printf("%-8s\t%-25s\t%-15s\t%s\n",
                    item.StableID,
                    title,
                    tags,
                    storagePath)
            }
        }
    } else {
        fmt.Printf("%-8s\t%-25s\t%-15s\t\n",
            item.StableID,
            title,
            tags)
    }
}

// List displays items matching the given filters
func (c *CLI) List(titleFilter, tagFilter string, verbose bool) error {
    items, err := c.repo.ListItems(titleFilter, tagFilter)
    if err != nil {
        return fmt.Errorf("listing items: %w", err)
    }

    for _, item := range items {
        c.printItem(item, verbose)
    }
    return nil
}

// Open launches the default application for the item's attachment
func (c *CLI) Open(stableID string) error {
    item, err := c.repo.GetByStableID(stableID)
    if err != nil {
        return fmt.Errorf("getting item: %w", err)
    }

    path := c.getStoragePath(item)
    if path == "" {
        return fmt.Errorf("no attachment found for item: %s", stableID)
    }

    cmd := exec.Command("open", path)
    if err := cmd.Run(); err != nil {
        return fmt.Errorf("opening file: %w", err)
    }
    return nil
}

// Reference generates a reference link for the item
func (c *CLI) Reference(stableID string) error {
    item, err := c.repo.GetByStableID(stableID)
    if err != nil {
        return fmt.Errorf("getting item: %w", err)
    }

    path := c.getStoragePath(item)
    if path == "" {
        return fmt.Errorf("no attachment found for item: %s", stableID)
    }

    tags := ""
    if item.Tags.Valid {
        tags = item.Tags.String
    }

    tags = "{" + tags + "}"
    if tags == "{}" {
        tags = "{}"
    }
    fmt.Printf("[zotero: %s, stableid: %s, tags: %s, version: %s](%s)\n",
        item.Title,
        item.StableID,
        tags,
        c.cfg.Version,
        path)
    return nil
}

func main() {
    cfg := Config{
        DBPath:      "/Users/vania/data/zotero/zotero.sqlite",
        StoragePath: "/Users/vania/data/zotero/storage/",
        Version:     "1.0",
    }

    titleFlag := flag.String("f", "", "Find items by title")
    tagFlag := flag.String("t", "", "Find items by tag")
    verboseFlag := flag.Bool("v", false, "Verbose output")
    flag.Parse()

    db, err := sql.Open("sqlite3", cfg.DBPath)
    if err != nil {
        log.Fatalf("Error opening database: %v", err)
    }
    defer db.Close()

    repo := NewRepository(db, cfg)
    cji := NewCLI(repo, cfg)

    args := flag.Args()
    if len(args) == 0 {
        if err := cli.List(*titleFlag, *tagFlag, *verboseFlag); err != nil {
            log.Fatalf("Error listing items: %v", err)
        }
        return
    }

    command := args[0]
    switch command {
    case "open":
        if len(args) != 2 {
            log.Fatal("Usage: store-zotero open <stableid>")
        }
        if err := cli.Open(args[1]); err != nil {
            log.Fatalf("Error opening item: %v", err)
        }

    case "reference":
        if len(args) != 2 {
            log.Fatal("Usage: store-zotero reference <stableid>")
        }
        if err := cli.Reference(args[1]); err != nil {
            log.Fatalf("Error generating reference: %v", err)
        }

    default:
        log.Fatalf("Unknown command: %s", command)
    }
}
