const copyButton = document.querySelector("#copy-command");
const command = document.querySelector("#install-command");
const feedback = document.querySelector("#copy-feedback");

if (copyButton && command && feedback) {
  copyButton.hidden = false;
  copyButton.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(command.textContent.trim());
      feedback.textContent = "Commands copied.";
    } catch {
      const range = document.createRange();
      range.selectNodeContents(command);
      const selection = window.getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      command.focus();
      feedback.textContent = "Commands selected. Copy them using your keyboard.";
    }
  });
}
