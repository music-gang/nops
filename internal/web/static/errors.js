// Error pages: a boosted link or form shows the page the server sent, whatever
// its status, as it does without JavaScript. htmx swaps only 2xx answers by
// default. The refresh of a live page is not boosted, so when it fails the page
// keeps what it shows.
document.addEventListener("htmx:beforeSwap", function (e) {
  if (e.detail.boosted) {
    e.detail.shouldSwap = true;
    e.detail.isError = false;
  }
});
