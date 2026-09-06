// Compile the generated transport itself, isolated from unrelated DTO codegen.
pub mod error {
    include!(concat!(env!("GENERATED_AUTH_ROOT"), "/rust/src/error.rs"));
}
pub mod auth {
    include!(concat!(env!("GENERATED_AUTH_ROOT"), "/rust/src/auth.rs"));
}
