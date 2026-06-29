#[allow(warnings)]
mod bindings;

use bindings::Guest;

struct Component;

impl Guest for Component {
    fn run(args: Vec<String>) -> Result<Vec<u8>, String> {
        let subject = args.first().map(String::as_str).unwrap_or("cerberus");
        Ok(format!("hello-shard:{subject}").into_bytes())
    }
}

bindings::export!(Component with_types_in bindings);
