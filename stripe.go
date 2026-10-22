package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Stripe-Anbindung über die REST-API (ohne SDK).
//
// Umgebungsvariablen:
//   STRIPE_SECRET_KEY       sk_live_… / sk_test_…
//   STRIPE_WEBHOOK_SECRET   whsec_…
//   STRIPE_PRICE_BASIS      price_… (monatlich, pro Standort)
//   STRIPE_PRICE_PRO        price_…
//   STRIPE_PRICE_PARTNER    price_…
//   STRIPE_AUTOMATIC_TAX=1  Stripe Tax verwenden (optional)

var stripeAPI = "https://api.stripe.com"

func stripeEnabled() bool { return os.Getenv("STRIPE_SECRET_KEY") != "" }

func stripePrice(plan string) string {
	return os.Getenv("STRIPE_PRICE_" + strings.ToUpper(plan))
}

func planForPrice(price string) string {
	for _, p := range plans {
		if price != "" && stripePrice(p.ID) == price {
			return p.ID
		}
	}
	return ""
}

var stripeClient = &http.Client{Timeout: 20 * time.Second}

func stripeCall(method, path string, form url.Values) (map[string]any, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, stripeAPI+path, body)
	req.Header.Set("Authorization", "Bearer "+os.Getenv("STRIPE_SECRET_KEY"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := stripeClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(b, &out)
	if resp.StatusCode >= 300 {
		msg := resp.Status
		if e, ok := out["error"].(map[string]any); ok {
			if m, ok := e["message"].(string); ok {
				msg = m
			}
		}
		return out, fmt.Errorf("Stripe: %s", msg)
	}
	return out, nil
}

func str(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

func num(m map[string]any, keys ...string) int64 {
	v, _ := strconv.ParseInt(str(m, keys...), 10, 64)
	return v
}

func (s *Server) billingOrg(w http.ResponseWriter, r *http.Request) *Org {
	p := principal(r)
	if !p.isOwner() {
		http.Error(w, "Nur der Inhaber kann das Abo ändern", 403)
		return nil
	}
	if !stripeEnabled() {
		http.Error(w, "Online-Zahlung ist nicht eingerichtet – bitte beim Anbieter melden", 400)
		return nil
	}
	o := s.m.orgByID(p.Org)
	if o == nil {
		http.Error(w, "Kunde unbekannt", 404)
		return nil
	}
	if o.Billing == "invoice" || o.Plan == "internal" {
		http.Error(w, "Dieses Konto wird per Rechnung abgerechnet – Änderungen bitte beim Anbieter anfragen", 400)
		return nil
	}
	return o
}

func quantityFor(plan Plan, sites int) int {
	if sites < plan.MinSites {
		sites = plan.MinSites
	}
	if sites < 1 {
		sites = 1
	}
	return sites
}

// Neues Abo über Stripe Checkout
func (s *Server) handleBillingCheckout(w http.ResponseWriter, r *http.Request) {
	o := s.billingOrg(w, r)
	if o == nil {
		return
	}
	var req struct {
		Plan  string `json:"plan"`
		Sites int    `json:"sites"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	pl := planByID(req.Plan)
	price := stripePrice(pl.ID)
	if pl.ID != req.Plan || !pl.Public || pl.PriceCents == 0 || price == "" {
		http.Error(w, "Paket nicht buchbar", 400)
		return
	}
	if o.StripeSub != "" && o.Status != "canceled" {
		http.Error(w, "Es besteht bereits ein Abo – bitte „Paket ändern“ verwenden", 409)
		return
	}
	used, _ := s.m.usage(o.ID)
	qty := quantityFor(pl, max(req.Sites, used))
	base := baseURL(r)
	f := url.Values{}
	f.Set("mode", "subscription")
	f.Set("line_items[0][price]", price)
	f.Set("line_items[0][quantity]", strconv.Itoa(qty))
	f.Set("success_url", base+"/#/billing?ok=1")
	f.Set("cancel_url", base+"/#/billing")
	f.Set("client_reference_id", o.ID)
	f.Set("metadata[org]", o.ID)
	f.Set("subscription_data[metadata][org]", o.ID)
	f.Set("subscription_data[metadata][plan]", pl.ID)
	f.Set("allow_promotion_codes", "true")
	f.Set("billing_address_collection", "required")
	f.Set("tax_id_collection[enabled]", "true")
	f.Set("locale", "de")
	if os.Getenv("STRIPE_AUTOMATIC_TAX") == "1" {
		f.Set("automatic_tax[enabled]", "true")
	}
	if o.StripeCustomer != "" {
		f.Set("customer", o.StripeCustomer)
		f.Set("customer_update[address]", "auto")
		f.Set("customer_update[name]", "auto")
	} else {
		if p := principal(r); p.User != nil {
			f.Set("customer_email", p.User.Email)
		} else if o.Email != "" {
			f.Set("customer_email", o.Email)
		}
		f.Set("customer_creation", "always")
	}
	res, err := stripeCall("POST", "/v1/checkout/sessions", f)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	writeJSON(w, map[string]string{"url": str(res, "url")})
}

// Paket oder Anzahl Standorte im laufenden Abo ändern (anteilig verrechnet)
func (s *Server) handleBillingChange(w http.ResponseWriter, r *http.Request) {
	o := s.billingOrg(w, r)
	if o == nil {
		return
	}
	var req struct {
		Plan  string `json:"plan"`
		Sites int    `json:"sites"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if o.StripeSub == "" || o.StripeItem == "" || o.Status == "canceled" {
		http.Error(w, "Kein laufendes Abo – bitte neu buchen", 400)
		return
	}
	pl := planByID(req.Plan)
	price := stripePrice(pl.ID)
	if pl.ID != req.Plan || !pl.Public || pl.PriceCents == 0 || price == "" {
		http.Error(w, "Paket nicht buchbar", 400)
		return
	}
	used, hosts := s.m.usage(o.ID)
	qty := quantityFor(pl, req.Sites)
	if qty < used {
		http.Error(w, fmt.Sprintf("Sie nutzen bereits %d Standorte – bitte zuerst Standorte löschen", used), 400)
		return
	}
	if pl.HostsPerSite > 0 && hosts > pl.HostsPerSite*qty {
		http.Error(w, fmt.Sprintf("Sie nutzen %d Hosts – im gewählten Paket sind %d möglich", hosts, pl.HostsPerSite*qty), 400)
		return
	}
	f := url.Values{}
	f.Set("items[0][id]", o.StripeItem)
	f.Set("items[0][price]", price)
	f.Set("items[0][quantity]", strconv.Itoa(qty))
	f.Set("proration_behavior", "create_prorations")
	f.Set("metadata[plan]", pl.ID)
	sub, err := stripeCall("POST", "/v1/subscriptions/"+url.PathEscape(o.StripeSub), f)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	s.applySubscription(sub)
	writeJSON(w, map[string]bool{"ok": true})
}

// Stripe-Kundenportal: Zahlungsart, Rechnungen, Kündigung
func (s *Server) handleBillingPortal(w http.ResponseWriter, r *http.Request) {
	o := s.billingOrg(w, r)
	if o == nil {
		return
	}
	if o.StripeCustomer == "" {
		http.Error(w, "Noch kein Abo gebucht", 400)
		return
	}
	f := url.Values{}
	f.Set("customer", o.StripeCustomer)
	f.Set("return_url", baseURL(r)+"/#/billing")
	f.Set("locale", "de")
	res, err := stripeCall("POST", "/v1/billing_portal/sessions", f)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	writeJSON(w, map[string]string{"url": str(res, "url")})
}

// ---------- Webhook ----------

func verifyStripeSignature(payload []byte, header, secret string, now time.Time) error {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts = kv[1]
		case "v1":
			sigs = append(sigs, kv[1])
		}
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return errors.New("Signatur fehlt")
	}
	if d := now.Unix() - t; d > 300 || d < -300 {
		return errors.New("Zeitstempel zu alt")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(want)) {
			return nil
		}
	}
	return errors.New("Signatur ungültig")
}

func (s *Server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	if secret == "" {
		http.Error(w, "nicht eingerichtet", 404)
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "zu groß", 400)
		return
	}
	if err := verifyStripeSignature(payload, r.Header.Get("Stripe-Signature"), secret, time.Now()); err != nil {
		log.Printf("Stripe-Webhook abgelehnt: %v", err)
		http.Error(w, err.Error(), 400)
		return
	}
	var ev struct {
		Type string `json:"type"`
		Data struct {
			Object map[string]any `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		http.Error(w, "ungültig", 400)
		return
	}
	obj := ev.Data.Object
	switch ev.Type {
	case "checkout.session.completed":
		org := str(obj, "client_reference_id")
		if org == "" {
			org = str(obj, "metadata", "org")
		}
		cust, subID := str(obj, "customer"), str(obj, "subscription")
		s.m.mu.Lock()
		for i := range s.m.cfg.Orgs {
			if s.m.cfg.Orgs[i].ID == org {
				s.m.cfg.Orgs[i].StripeCustomer = cust
				s.m.cfg.Orgs[i].StripeSub = subID
				s.m.cfg.Orgs[i].Billing = "stripe"
				if e := str(obj, "customer_details", "email"); e != "" {
					s.m.cfg.Orgs[i].Email = e
				}
			}
		}
		s.m.saveConfigLocked()
		s.m.mu.Unlock()
		if subID != "" {
			if sub, err := stripeCall("GET", "/v1/subscriptions/"+url.PathEscape(subID), nil); err == nil {
				s.applySubscription(sub)
			} else {
				log.Printf("Stripe: Abo %s nicht abrufbar: %v", subID, err)
			}
		}
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		s.applySubscription(obj)
	case "invoice.payment_failed":
		s.setOrgStatusByCustomer(str(obj, "customer"), "past_due")
	}
	w.WriteHeader(200)
}

// applySubscription überträgt Paket, Menge und Status eines Stripe-Abos auf die Org.
func (s *Server) applySubscription(sub map[string]any) {
	subID := str(sub, "id")
	status := str(sub, "status")
	cust := str(sub, "customer")
	org := str(sub, "metadata", "org")
	var itemID, price string
	var qty, periodEnd int64
	if items, ok := sub["items"].(map[string]any); ok {
		if data, ok := items["data"].([]any); ok && len(data) > 0 {
			if it, ok := data[0].(map[string]any); ok {
				itemID = str(it, "id")
				price = str(it, "price", "id")
				qty = num(it, "quantity")
				periodEnd = num(it, "current_period_end")
			}
		}
	}
	if periodEnd == 0 {
		periodEnd = num(sub, "current_period_end")
	}
	plan := planForPrice(price)
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	for i := range s.m.cfg.Orgs {
		o := &s.m.cfg.Orgs[i]
		if !(o.ID == org || (subID != "" && o.StripeSub == subID) || (org == "" && cust != "" && o.StripeCustomer == cust)) {
			continue
		}
		o.StripeSub, o.StripeItem, o.Billing = subID, itemID, "stripe"
		if cust != "" {
			o.StripeCustomer = cust
		}
		if periodEnd > 0 {
			o.PaidUntil = periodEnd * 1000
		}
		switch status {
		case "active", "trialing":
			o.Status = "active"
			if plan != "" {
				o.Plan = plan
				o.TrialUntil, o.TrialEnded = 0, false // bezahltes Abo beendet die Testphase
			}
			if qty > 0 {
				o.Sites = int(qty)
			}
		case "past_due":
			o.Status = "past_due"
		case "canceled", "unpaid", "incomplete_expired":
			o.Status = "canceled"
			o.Plan = "free"
			o.Sites = 0
		}
		log.Printf("Abo %s für %s: %s, Paket %s, %d Standort(e)", subID, o.Name, status, o.Plan, o.Sites)
	}
	s.m.saveConfigLocked()
}

func (s *Server) setOrgStatusByCustomer(cust, status string) {
	if cust == "" {
		return
	}
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	for i := range s.m.cfg.Orgs {
		if s.m.cfg.Orgs[i].StripeCustomer == cust {
			s.m.cfg.Orgs[i].Status = status
		}
	}
	s.m.saveConfigLocked()
}
