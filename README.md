This project generates a summary of all available repertoires from all cinemas in the city of Kraków, while highlighting any movies that have been added to those repertoires recently, based on previously generated summaries stored in a local database.

The summary can be stored as a textfile, if provided with flag `-log`, or sent as a notification via gotify, if provided with options `-origin` (address of the gotify server) and `-token` (gotify app token).
So e.g:

`main -log -origin="http://localhost:80" -token="VXfxf84GDD.MXX"`

I personally have it automated and ran periodically, with the notifications sent to the gotify app on my phone.

The repertoires are obtained either via web scraping, for the following cinemas:
- Agrafka
- Kijów
-	Kika
-	Mikro
-	Pod Baranami
-	Paradox
-	Sfinks

Or via calls using reverse engineered APIs, for the following cinemas:
- Cinema City Bonarka
- Cinema City Kazimierz
- Cinema City Zakopianka
- Multikino
