### Security

- A repository the routing model picked is cloned without asking only when your message names that exact repository (LOOM-152). Before, its URL merely had to appear inside the message, so naming `…/tools-fork` let the model clone `…/tools` unasked. A trailing `/` or `.git` still counts as the same repository.
