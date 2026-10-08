// Copy button: <button data-copy="id"> copies the value of the input with that id.
// The Clipboard API needs a secure context (https or localhost). Without one the
// input is left selected, to copy by hand.
document.addEventListener("click", function (e) {
  var button = e.target.closest("[data-copy]");
  if (!button) {
    return;
  }
  var input = document.getElementById(button.getAttribute("data-copy"));
  if (!input) {
    return;
  }
  input.select();
  if (navigator.clipboard) {
    navigator.clipboard.writeText(input.value).then(function () {
      button.textContent = "Copied";
    });
  }
});
