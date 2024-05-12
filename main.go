package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/joho/godotenv/autoload"
	qbo "github.com/mkcr-innovations/quickbooksonline-go-client/pkg/client"
	"github.com/mkcr-innovations/quickbooksonline-go-client/pkg/types"
	"github.com/sethvargo/go-envconfig"
	"golang.org/x/oauth2"
)

type Config struct {
	QuickbooksClientID     string `env:"QUICKBOOKS_CLIENT_ID, required"`
	QuickbooksClientSecret string `env:"QUICKBOOKS_CLIENT_SECRET, required"`

	// TODO: make these optional and do oauth flow if missing
	QuickBooksCallbackBaseURL string `env:"QUICKBOOKS_CALLBACK_BASE_URL, required"`
	QuickbooksRefreshToken    string `env:"QUICKBOOKS_REFRESH_TOKEN, required"`
	QuickbooksRealmID         string `env:"QUICKBOOKS_REALM_ID, required"`
}

/*
Deposits - currMonth/lastMonth/year/allTime
Purchases - currMonth/lastMonth/year/allTime
Accounts - currBal
Project Class - [Amount left in budget]
	- allTime spend vs Approved
	> List Classes in Active Budget
		> Get Approved Amount from Name
	> TotalAmt  from Purchase lines with Class name
	> TotalAmt - Approved == Total Remaining
Recurring Class - [Spend per month] or [Spend per year (SIG-Networking)]
	- SIG Budgets
	- Manager Budgets
	- Hack Denhac Day
Other Recurring - [Quarter/Year Recurring]
Pools - [Budgets with Max Cap]
	- Vending Machine
	- Merch
*/

type Report struct {
	Deposits  []types.Deposit  `json:"deposits"`
	Purchases []types.Purchase `json:"purchases"`
	Accounts  []Account        `json:"accounts"`
	Classes   []types.Class    `json:"classes"`
}

type Account struct {
	Name           string  `json:"name"`
	CurrentBalance float64 `json:"current_balance"`
}

type Purchase struct {
}

type QBService struct {
	rts       RefreshTokenStorer
	oauth2Cfg *oauth2.Config
}

func (q *QBService) FetchAccounts(ctx context.Context) ([]Account, error) {
	client, err := q.getClient(ctx)
	if err != nil {
		return nil, err
	}
	res, err := client.Account.Query().Where("AccountType = 'Bank' AND Active = true AND Name != 'Change Machine'").Exec()
	if err != nil {
		return nil, err
	}

	var accounts []Account
	for _, a := range res.QueryResponse.Account {
		na := Account{
			Name:           a.Name,
			CurrentBalance: a.CurrentBalance,
		}
		accounts = append(accounts, na)
	}

	return accounts, nil
}

func (q *QBService) FetchPurchases(ctx context.Context) ([]types.Purchase, error) {
	client, err := q.getClient(ctx)
	if err != nil {
		return nil, err
	}

	start, stop := lastDatePeriod()
	res, err := client.Purchase.Query().
		Where(fmt.Sprintf("TxnDate > '%s' AND TxnDate < '%s'", start.Format(time.DateOnly), stop.Format(time.DateOnly))).
		MaxResults(1000). // TODO: https://github.com/mkcr-innovations/quickbooksonline-go-client/issues/3
		Exec()
	if err != nil {
		return nil, err
	}
	return res.QueryResponse.Purchase, err
}

func (q *QBService) FetchDeposits(ctx context.Context) ([]types.Deposit, error) {
	client, err := q.getClient(ctx)
	if err != nil {
		return nil, err
	}

	start, stop := lastDatePeriod()
	res, err := client.Deposit.Query().
		Where(fmt.Sprintf("TxnDate > '%s' AND TxnDate < '%s'", start.Format(time.DateOnly), stop.Format(time.DateOnly))).
		MaxResults(1000). // TODO: https://github.com/mkcr-innovations/quickbooksonline-go-client/issues/3
		Exec()
	if err != nil {
		return nil, err
	}
	return res.QueryResponse.Deposit, err
}

func (q *QBService) FetchClasses(ctx context.Context) ([]types.Class, error) {
	client, err := q.getClient(ctx)
	if err != nil {
		return nil, err
	}
	res, err := client.Class.Query().
		MaxResults(1000). // TODO: https://github.com/mkcr-innovations/quickbooksonline-go-client/issues/3
		Exec()
	if err != nil {
		return nil, err
	}
	return res.QueryResponse.Class, err
}

func lastDatePeriod() (time.Time, time.Time) {
	now := time.Now()
	return now.AddDate(0, 0, -28), now
}

func firstAndLastOfMonth() (time.Time, time.Time) {
	now := time.Now()
	first := now.AddDate(0, 0, -now.Day()+1)
	last := now.AddDate(0, 1, -now.Day())
	return first, last
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	ctx := context.Background()

	var cfg Config
	err := envconfig.Process(ctx, &cfg)
	if err != nil {
		panic(err)
	}

	scopes := []string{"com.intuit.quickbooks.accounting"}
	endpoint := oauth2.Endpoint{
		AuthURL:   "https://appcenter.intuit.com/connect/oauth2",
		TokenURL:  "https://oauth.platform.intuit.com/oauth2/v1/tokens/bearer",
		AuthStyle: oauth2.AuthStyleInParams,
	}
	oauthConfig := oauth2.Config{
		ClientID:     cfg.QuickbooksClientID,
		ClientSecret: cfg.QuickbooksClientSecret,
		Endpoint:     endpoint,
		RedirectURL:  fmt.Sprintf("%s/callback", cfg.QuickBooksCallbackBaseURL),
		Scopes:       scopes,
	}

	dts := &DiskTokenStorage{
		Filename: ".token",
	}

	qbs := &QBService{
		rts:       dts,
		oauth2Cfg: &oauthConfig,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		var report Report

		accounts, err := qbs.FetchAccounts(ctx)
		if err != nil {
			log.Error("unable to fetch accounts", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		report.Accounts = accounts

		purchases, err := qbs.FetchPurchases(ctx)
		if err != nil {
			log.Error("unable to fetch purchases", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		report.Purchases = purchases

		deposits, err := qbs.FetchDeposits(ctx)
		if err != nil {
			log.Error("unable to fetch deposits", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		report.Deposits = deposits

		classes, err := qbs.FetchClasses(ctx)
		if err != nil {
			log.Error("unable to fetch classes", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		report.Classes = classes

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(report)
	})

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, oauthConfig.AuthCodeURL("state"), http.StatusSeeOther)
	})

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")
		realmID := r.URL.Query().Get("realmId")

		log.Info("GET /callback", "code", code, "state", state, "realmId", realmID)

		if code == "" || state == "" || realmID == "" {
			log.Info("missing callback data")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		token, err := oauthConfig.Exchange(r.Context(), code)
		if err != nil {
			log.Error("unable to exchange token", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		log.Info("got token", "token", token)
		w.WriteHeader(http.StatusOK)
	})

	log.Info("starting server")
	err = http.ListenAndServe(":8080", mux)
	log.Error("error with server", err)
}

type RefreshTokenStorer interface {
	Get() (string, error)
	Put(string) error
}

type DiskTokenStorage struct {
	Filename string
	mu       sync.Mutex
}

var _ RefreshTokenStorer = &DiskTokenStorage{}

func (d *DiskTokenStorage) Get() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, err := os.ReadFile(d.Filename)
	if err != nil {
		return "", err
	}
	rt := strings.TrimSpace(string(b))
	if rt == "" {
		return "", fmt.Errorf("refresh token not found in: %s", d.Filename)
	}
	return rt, nil
}

func (d *DiskTokenStorage) Put(rt string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := os.WriteFile(d.Filename, []byte(rt), 0660)
	if err != nil {
		return err
	}
	return nil
}

func (q *QBService) getClient(ctx context.Context) (*qbo.Client, error) {
	var cfg Config
	err := envconfig.Process(ctx, &cfg)
	if err != nil {
		return nil, err
	}

	rt, err := q.rts.Get()
	if err != nil {
		return nil, err
	}

	ts := q.oauth2Cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: rt})
	t, err := ts.Token()
	if err != nil {
		return nil, err
	}

	if t.RefreshToken != rt {
		err := q.rts.Put(t.RefreshToken)
		if err != nil {
			return nil, err
		}
	}

	httpClient := oauth2.NewClient(ctx, ts)

	qbClient := qbo.NewClient(httpClient, cfg.QuickbooksRealmID)
	return qbClient, nil
}
