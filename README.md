# parkingapp

Telegram-Bot für Parkplätze von [fair parken](https://www.fairparken.com/de-DE/pay) mit FreeFlow (Kennzeichenerkennung, ohne Ticket).

- Mehrere Nutzer, jeder verwaltet eigene Kennzeichen
- Push-Nachricht, sobald ein Parkvorgang für ein Kennzeichen erkannt wird, und wenn er nicht mehr offen ist
- Erinnerung kurz vor Ablauf der 48-h-Frist für die Online-Zahlung
- Aktive Abfrage (`/check`) und Tarife mit Preisen für den laufenden Parkvorgang (`/tarife`)
- Bezahl-Button öffnet die offizielle Seite mit vorausgefülltem Kennzeichen – der Bot berührt keine Zahlungsdaten

## Befehle

| Befehl | Funktion |
|---|---|
| `/add HH-AB 123 Name` | Kennzeichen überwachen (Name optional) |
| `/list` | eigene Kennzeichen mit Buttons |
| `/check [Kennzeichen]` | jetzt prüfen; ein Kennzeichen ohne Befehl schicken geht auch |
| `/tarife [Kennzeichen \| Parkplatz-ID]` | Tarife; Preise nur bei offenem Parkvorgang |
| `/bezahlen [Kennzeichen]` | Link zur Bezahlseite |
| `/remove [Kennzeichen]` | Kennzeichen entfernen |

## Betrieb

```sh
cp .env.example .env   # TELEGRAM_BOT_TOKEN eintragen (von @BotFather)
docker compose up -d --build
docker compose logs -f
```

Die SQLite-Datenbank liegt im Volume `data`. Konfiguration siehe `.env.example`.
`ALLOWED_USER_IDS` beschränkt den Bot auf bestimmte Telegram-Nutzer; Unbekannte bekommen ihre ID angezeigt.

Entwicklung: `go test ./...`, Live-Test gegen die echte API: `FAIRPARKEN_LIVE=1 go test ./internal/fairparken -run Live`.

## fair-parken-API

Es gibt keine offizielle API. Die Endpunkte stammen aus dem JavaScript des Web-Clients `https://pay.fairparken.com` und können sich ohne Ankündigung ändern.

| Endpunkt | Verwendung |
|---|---|
| `POST api/AnprResults/AnprResultSearch` `{"plate":"HHAB123","voucherCode":""}` | offene Parkvorgänge eines Kennzeichens, `[]` wenn keiner |
| `GET api/AnprResults/GetTariffs/{quotaID}` | Tarife (Fahrzeugklassen: `icon` = `null`/Auto, `Wohnmobil`, `Bus`, `Motorrad`) |
| `POST api/Payment/ApplyVoucherCode` (Parkvorgang mit `tariffId`) | Preis für einen Tarif neu berechnen; nutzt der Web-Client bei der Tarifwahl. Der Bot stellt danach den ursprünglichen Tarif wieder her. |
| `https://pay.fairparken.com/plate/{Kennzeichen}` | Bezahlseite mit vorausgefülltem Kennzeichen |

Felder eines Treffers (`anprResult`): `plate`, `areaGkz`, `quotaID`, `quotaAddress`, `tariffId`, `realUTC_IN`, `realUTC_OUT`, `price`, `originalPrice`, `voucherValue`, `orderType`.

Offene Punkte, erst mit einem echten Parkvorgang prüfbar:

- Ob die Suche schon während des Parkens einen Treffer liefert (die Website bietet Zahlung vor der Ausfahrt an, daher wahrscheinlich).
- `price` wird als Euro interpretiert (der Web-Client rechnet `price * 100` in Cent um).
- Was `realUTC_OUT` vor der Ausfahrt enthält. Die Erinnerung greift nur, wenn die Ausfahrtszeit fest ist.

Abfrage-Last: ein Request pro überwachtem Kennzeichen alle `POLL_INTERVAL` (Standard 3 min, mit Jitter), dazwischen `REQUEST_GAP`.
